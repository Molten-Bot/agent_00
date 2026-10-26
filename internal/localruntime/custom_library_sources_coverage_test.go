package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
)

func TestCustomLibrarySourcesDefaults(t *testing.T) {
	t.Setenv("HARNESS_RUNTIME_CONFIG_PATH", "")

	var s CustomLibrarySources
	before := time.Now().UTC()
	got := s.now()
	after := time.Now().UTC()
	if got.Before(before) || got.After(after) {
		t.Fatalf("now() = %v, want between %v and %v", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("now() location = %v, want UTC", got.Location())
	}

	if gotPath, want := s.path(), defaultRuntimeConfigPath(); gotPath != want {
		t.Fatalf("path() = %q, want default %q", gotPath, want)
	}

	if got := s.syncer(); got != (customlibrary.Syncer{}) {
		t.Fatalf("syncer() = %#v, want zero-value customlibrary.Syncer{}", got)
	}

	// logf with no Logf callback configured must be a safe no-op.
	s.logf("unused format %s", "arg")
}

func TestCustomLibrarySourcesFromDocMissingKey(t *testing.T) {
	t.Parallel()
	result, err := customLibrarySourcesFromDoc(map[string]any{})
	if err != nil {
		t.Fatalf("customLibrarySourcesFromDoc(no key) error = %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("result = %+v, want empty map", result)
	}
}

func TestCustomLibrarySourcesFromDocMalformedShape(t *testing.T) {
	t.Parallel()
	_, err := customLibrarySourcesFromDoc(map[string]any{"custom_library_sources": "not-a-map"})
	if err == nil || !strings.Contains(err.Error(), "read custom library sources") {
		t.Fatalf("customLibrarySourcesFromDoc(wrong shape) error = %v, want read custom library sources failure", err)
	}
}

func TestCustomLibrarySourcesFromDocRejectsKeyNameMismatch(t *testing.T) {
	t.Parallel()
	_, err := customLibrarySourcesFromDoc(map[string]any{
		"custom_library_sources": map[string]any{
			"acme": map[string]any{"name": "other-name", "repo": "git@github.com:acme/docs.git"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid persisted custom library source") {
		t.Fatalf("customLibrarySourcesFromDoc(key mismatch) error = %v, want invalid persisted source", err)
	}
}

func TestCustomLibrarySourcesTransactionPropagatesLoadError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &CustomLibrarySources{Path: path}
	if _, err := store.List(); err == nil {
		t.Fatal("List(malformed config) error = nil, want load failure")
	}
}

func TestCustomLibrarySourcesTransactionPropagatesInvalidPersistedEntries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"custom_library_sources":{"acme":{"name":"other","repo":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &CustomLibrarySources{Path: path}
	if _, err := store.List(); err == nil {
		t.Fatal("List(invalid persisted entries) error = nil, want validation failure")
	}
}

func TestCustomLibrarySourcesTransactionPropagatesWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	store := &CustomLibrarySources{Path: filepath.Join(dir, "config.json"), Init: InitConfig{Version: "v1"}, Syncer: &fakeCustomLibrarySyncer{}}
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "repo"}); err == nil {
		t.Fatal("Add(unwritable dir) error = nil, want write failure")
	}
}

func TestCustomLibrarySourcesAddRejectsInvalidSource(t *testing.T) {
	t.Parallel()
	store, _ := newCustomLibrarySourcesTestStore(t, &fakeCustomLibrarySyncer{})
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "Not Valid!", Repo: "repo"}); err == nil {
		t.Fatal("Add(invalid name) error = nil, want validation failure")
	}
}

func TestCustomLibrarySourcesRemoveRejectsEmptyName(t *testing.T) {
	t.Parallel()
	store, _ := newCustomLibrarySourcesTestStore(t, &fakeCustomLibrarySyncer{})
	if err := store.Remove("   "); err == nil {
		t.Fatal("Remove(blank name) error = nil, want error")
	}
}

// syncerWithFailingRemove lets Remove's cache-cleanup warning branch be
// exercised without touching the shared fakeCustomLibrarySyncer fixture used
// by other tests in this package.
type syncerWithFailingRemove struct{}

func (syncerWithFailingRemove) Sync(_ context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
	return customlibrary.SyncResult{Source: src, CommitSHA: "sha", TaskCount: 1, Changed: true}, nil
}

func (syncerWithFailingRemove) RemoveSource(string) error {
	return errors.New("cache lock held")
}

func TestCustomLibrarySourcesRemoveLogsCacheCleanupFailure(t *testing.T) {
	t.Parallel()
	store := &CustomLibrarySources{
		Path:   filepath.Join(t.TempDir(), "config.json"),
		Init:   InitConfig{Version: "v1"},
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		Syncer: syncerWithFailingRemove{},
	}
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "repo"}); err != nil {
		t.Fatal(err)
	}

	var logs []string
	store.Logf = func(format string, args ...any) { logs = append(logs, format) }

	if err := store.Remove("acme"); err != nil {
		t.Fatalf("Remove() error = %v, want nil (cache cleanup failure must not fail Remove)", err)
	}
	if len(logs) == 0 {
		t.Fatal("Remove() did not log the cache cleanup failure")
	}
}

func TestCustomLibrarySourcesSyncAllPropagatesListError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	store := &CustomLibrarySources{Path: path, Logf: func(format string, args ...any) { logs = append(logs, format) }}
	// Must not panic even though List() fails internally.
	store.SyncAll(context.Background())
	if len(logs) == 0 {
		t.Fatal("SyncAll(List error) did not log the failure")
	}
}

func TestCustomLibrarySourcesRunTicksRepeatedly(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{}
	store, _ := newCustomLibrarySourcesTestStore(t, syncer)
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "repo"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		syncer.mu.Lock()
		calls := len(syncer.calls)
		syncer.mu.Unlock()
		// One sync call happens immediately; wait for at least one more from
		// the ticker to confirm Run's loop actually re-syncs over time.
		if calls > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run() never performed a second, ticker-driven sync")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after context cancellation")
	}
}

func TestCustomLibrarySourcesFromDocMarshalError(t *testing.T) {
	t.Parallel()
	// A channel value cannot be marshaled to JSON, exercising the json.Marshal
	// error branch (distinct from the json.Unmarshal error branch covered by
	// TestCustomLibrarySourcesFromDocMalformedShape).
	_, err := customLibrarySourcesFromDoc(map[string]any{"custom_library_sources": make(chan int)})
	if err == nil {
		t.Fatal("customLibrarySourcesFromDoc(unmarshalable value) error = nil, want marshal failure")
	}
}

func TestCustomLibrarySourcesFromDocNullValueYieldsEmptyMap(t *testing.T) {
	t.Parallel()
	// "custom_library_sources": null marshals/unmarshals into a nil map,
	// exercising the nil-to-empty-map normalization branch.
	var raw any
	if err := json.Unmarshal([]byte("null"), &raw); err != nil {
		t.Fatal(err)
	}
	result, err := customLibrarySourcesFromDoc(map[string]any{"custom_library_sources": raw})
	if err != nil {
		t.Fatalf("customLibrarySourcesFromDoc(null) error = %v", err)
	}
	if result == nil || len(result) != 0 {
		t.Fatalf("result = %#v, want non-nil empty map", result)
	}
}

// cancelingSyncer cancels the supplied context as a side effect of its first
// Sync call, letting a test deterministically exercise SyncAll's mid-loop
// ctx.Err() guard with a second configured source that must never be synced.
type cancelingSyncer struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	calls  []string
}

func (s *cancelingSyncer) Sync(_ context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, src.Name)
	s.mu.Unlock()
	s.cancel()
	return customlibrary.SyncResult{Source: src, CommitSHA: "sha-" + src.Name, TaskCount: 1}, nil
}

func (s *cancelingSyncer) RemoveSource(string) error { return nil }

func TestCustomLibrarySourcesSyncAllStopsOnContextCancelMidLoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	syncer := &cancelingSyncer{cancel: cancel}
	store, _ := newCustomLibrarySourcesTestStore(t, &fakeCustomLibrarySyncer{})
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "a-first", Repo: "repo-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "b-second", Repo: "repo-b"}); err != nil {
		t.Fatal(err)
	}
	// List() sorts by name, so "a-first" syncs before "b-second"; cancel
	// during the first call and confirm the second is skipped entirely.
	store.Syncer = syncer

	store.SyncAll(ctx)

	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if len(syncer.calls) != 1 || syncer.calls[0] != "a-first" {
		t.Fatalf("calls = %v, want exactly [a-first] (loop must stop once ctx is canceled)", syncer.calls)
	}
}

func TestCustomLibrarySourcesSyncAllSkipsConcurrentlyRemovedEntry(t *testing.T) {
	t.Parallel()
	store, _ := newCustomLibrarySourcesTestStore(t, &fakeCustomLibrarySyncer{})
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "repo"}); err != nil {
		t.Fatal(err)
	}

	path := store.Path
	removingSyncer := &syncerFunc{fn: func(_ context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
		// Simulate a concurrent Remove() finishing between this SyncAll's
		// List() snapshot and its per-entry transaction by rewriting the
		// runtime config file without this source, bypassing the in-process
		// mutex the way a separate process restart would.
		if err := os.WriteFile(path, []byte(`{"version":"v1"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return customlibrary.SyncResult{Source: src, CommitSHA: "sha", TaskCount: 1}, nil
	}}
	store.Syncer = removingSyncer

	store.SyncAll(context.Background())

	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none (concurrently removed entry must not be re-added)", entries)
	}
}

type syncerFunc struct {
	fn func(context.Context, customlibrary.Source) (customlibrary.SyncResult, error)
}

func (s *syncerFunc) Sync(ctx context.Context, src customlibrary.Source) (customlibrary.SyncResult, error) {
	return s.fn(ctx, src)
}
func (s *syncerFunc) RemoveSource(string) error { return nil }

func TestCustomLibrarySourcesSyncAllNotifiesOnChange(t *testing.T) {
	t.Parallel()
	syncer := &fakeCustomLibrarySyncer{results: map[string]customlibrary.SyncResult{"acme": {CommitSHA: "sha-1", TaskCount: 1, Changed: true}}}
	store, changed := newCustomLibrarySourcesTestStore(t, syncer)
	if _, err := store.Add(context.Background(), customlibrary.Source{Name: "acme", Repo: "repo"}); err != nil {
		t.Fatal(err)
	}
	*changed = 0

	store.SyncAll(context.Background())

	if *changed != 1 {
		t.Fatalf("OnChanged called %d times, want 1 after a sync reports Changed=true", *changed)
	}
}

func TestCustomLibrarySourcesRunDefaultsNonPositiveInterval(t *testing.T) {
	t.Parallel()
	store, _ := newCustomLibrarySourcesTestStore(t, &fakeCustomLibrarySyncer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A non-positive interval must fall back to a safe default (1h)
		// instead of ticking immediately or panicking on NewTicker.
		store.Run(ctx, 0)
		close(done)
	}()
	// Give Run's initial SyncAll pass a moment to execute before canceling.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run(interval<=0) did not return promptly after context cancellation")
	}
}
