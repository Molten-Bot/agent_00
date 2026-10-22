package localruntime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
)

type fakeCustomLibrarySyncer struct {
	mu      sync.Mutex
	results map[string]customlibrary.SyncResult
	errs    map[string]error
	calls   []string
	removed []string
}

func (f *fakeCustomLibrarySyncer) Sync(_ context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, src.Name)
	if err := f.errs[src.Name]; err != nil {
		return customlibrary.SyncResult{}, err
	}
	if res, ok := f.results[src.Name]; ok {
		return res, nil
	}
	return customlibrary.SyncResult{Source: src, CommitSHA: "sha-" + src.Name, TaskCount: 1, Changed: true}, nil
}

func (f *fakeCustomLibrarySyncer) RemoveSource(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
}

func newCustomLibrarySourcesTestStore(t *testing.T, syncer *fakeCustomLibrarySyncer) (*CustomLibrarySources, *int) {
	t.Helper()
	changedCount := 0
	store := &CustomLibrarySources{
		Path:      filepath.Join(t.TempDir(), "config.json"),
		Init:      InitConfig{Version: "v1"},
		Now:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		Syncer:    syncer,
		OnChanged: func() { changedCount++ },
	}
	return store, &changedCount
}

func TestCustomLibrarySourcesAddPersistsAndSyncs(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{}
	store, changed := newCustomLibrarySourcesTestStore(t, syncer)

	entry, err := store.Add(context.Background(), customlibrary.Source{Name: "Acme", Repo: "git@github.com:acme/docs.git"})
	if err != nil {
		t.Fatalf("Add() error: %v", err)
	}
	if entry.Name != "acme" || entry.CommitSHA != "sha-acme" || entry.TaskCount != 1 || entry.SyncedAt == nil {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if *changed != 1 {
		t.Fatalf("OnChanged called %d times, want 1", *changed)
	}

	restarted := &CustomLibrarySources{Path: store.Path, Init: store.Init}
	entries, err := restarted.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "acme" {
		t.Fatalf("persisted entries = %+v", entries)
	}
}

func TestCustomLibrarySourcesAddDoesNotPersistOnSyncFailure(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{errs: map[string]error{"acme": errors.New("permission denied")}}
	store, changed := newCustomLibrarySourcesTestStore(t, syncer)

	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "git@github.com:acme/docs.git"}); err == nil {
		t.Fatalf("Add() = nil error, want sync failure surfaced")
	}
	if *changed != 0 {
		t.Fatalf("OnChanged called %d times, want 0", *changed)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none persisted after failed add", entries)
	}
}

func TestCustomLibrarySourcesRemoveDeletesAndClearsCache(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{}
	store, _ := newCustomLibrarySourcesTestStore(t, syncer)

	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "git@github.com:acme/docs.git"}); err != nil {
		t.Fatalf("Add() error: %v", err)
	}
	if err := store.Remove("acme"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none after Remove", entries)
	}
	if len(syncer.removed) != 1 || syncer.removed[0] != "acme" {
		t.Fatalf("syncer.removed = %v, want [acme]", syncer.removed)
	}
	if err := store.Remove("acme"); err == nil {
		t.Fatalf("Remove() of unknown source = nil error, want error")
	}
}

func TestCustomLibrarySourcesSyncAllKeepsFailingSourceWithError(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{}
	store, changed := newCustomLibrarySourcesTestStore(t, syncer)

	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "good", Repo: "repo-good"}); err != nil {
		t.Fatalf("Add(good) error: %v", err)
	}
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "bad", Repo: "repo-bad"}); err != nil {
		t.Fatalf("Add(bad) error: %v", err)
	}
	*changed = 0

	syncer.errs = map[string]error{"bad": errors.New("network unreachable")}
	syncer.results = map[string]customlibrary.SyncResult{"good": {CommitSHA: "sha-good", TaskCount: 2, Changed: false}}

	store.SyncAll(context.Background())

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want both sources kept despite one failing", entries)
	}
	byName := map[string]CustomLibrarySource{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if byName["bad"].LastError == "" {
		t.Fatalf("bad source LastError not recorded: %+v", byName["bad"])
	}
	if byName["good"].LastError != "" || byName["good"].TaskCount != 2 {
		t.Fatalf("good source unexpectedly affected: %+v", byName["good"])
	}
	// "good" reported Changed=false, "bad" failed outright, so no catalog
	// change happened - OnChanged should not fire again.
	if *changed != 0 {
		t.Fatalf("OnChanged called %d times, want 0", *changed)
	}
}

func TestCustomLibrarySourcesRunReturnsOnContextCancel(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{}
	store, _ := newCustomLibrarySourcesTestStore(t, syncer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		store.Run(ctx, time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run() did not return after context cancellation")
	}
}
