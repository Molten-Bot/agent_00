package localruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/customlibrary"
)

// CustomLibrarySource is one configured external library source plus the
// status of its most recent sync.
type CustomLibrarySource struct {
	customlibrary.Source
	CommitSHA string     `json:"commitSha,omitempty"`
	TaskCount int        `json:"taskCount"`
	SyncedAt  *time.Time `json:"syncedAt,omitempty"`
	LastError string     `json:"lastError,omitempty"`
}

// customLibrarySyncer matches customlibrary.Syncer, so tests can fake git/disk access.
type customLibrarySyncer interface {
	Sync(ctx context.Context, src customlibrary.Source) (customlibrary.SyncResult, error)
	RemoveSource(name string) error
}

// CustomLibrarySources persists configured custom library sources and keeps
// them synced: once immediately when a source is added, and again on every
// Run tick (hourly in production). OnChanged fires after a sync actually
// changes a source's generated catalog, so callers can push a UI refresh.
type CustomLibrarySources struct {
	Path      string
	Init      InitConfig
	Now       func() time.Time
	Syncer    customLibrarySyncer
	OnChanged func()
	Logf      func(string, ...any)
	mu        sync.Mutex
}

func (s *CustomLibrarySources) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *CustomLibrarySources) path() string {
	if strings.TrimSpace(s.Path) == "" {
		return defaultRuntimeConfigPath()
	}
	return s.Path
}

func (s *CustomLibrarySources) syncer() customLibrarySyncer {
	if s.Syncer != nil {
		return s.Syncer
	}
	return customlibrary.Syncer{}
}

func (s *CustomLibrarySources) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *CustomLibrarySources) notifyChanged() {
	if s.OnChanged != nil {
		s.OnChanged()
	}
}

func customLibrarySourcesFromDoc(doc map[string]any) (map[string]CustomLibrarySource, error) {
	result := map[string]CustomLibrarySource{}
	raw, ok := doc["custom_library_sources"]
	if !ok {
		return result, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("read custom library sources: %w", err)
	}
	if result == nil {
		result = map[string]CustomLibrarySource{}
	}
	for key, entry := range result {
		if key == "" || entry.Name != key {
			return nil, fmt.Errorf("invalid persisted custom library source %q", key)
		}
	}
	return result, nil
}

func (s *CustomLibrarySources) transaction(change func(map[string]CustomLibrarySource) error) ([]CustomLibrarySource, error) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	doc, err := loadRuntimeConfigDoc(s.path(), s.Init)
	if err != nil {
		return nil, err
	}
	sources, err := customLibrarySourcesFromDoc(doc)
	if err != nil {
		return nil, err
	}
	if change != nil {
		if err := change(sources); err != nil {
			return nil, err
		}
		if len(sources) == 0 {
			delete(doc, "custom_library_sources")
		} else {
			doc["custom_library_sources"] = sources
		}
		if err := writeRuntimeConfigDoc(s.path(), doc); err != nil {
			return nil, err
		}
	}
	result := make([]CustomLibrarySource, 0, len(sources))
	for _, entry := range sources {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// List returns every configured custom library source, sorted by name.
func (s *CustomLibrarySources) List() ([]CustomLibrarySource, error) { return s.transaction(nil) }

// Add validates and synchronously syncs src before persisting it, so adding
// a source either works right away or fails with a clear reason - it never
// saves a source that has not been proven reachable.
func (s *CustomLibrarySources) Add(ctx context.Context, src customlibrary.Source) (CustomLibrarySource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	src, err := customlibrary.ValidateSource(src)
	if err != nil {
		return CustomLibrarySource{}, err
	}
	result, err := s.syncer().Sync(ctx, src)
	if err != nil {
		return CustomLibrarySource{}, fmt.Errorf("could not sync %s: %w", src.Repo, err)
	}

	now := s.now()
	entry := CustomLibrarySource{Source: src, CommitSHA: result.CommitSHA, TaskCount: result.TaskCount, SyncedAt: &now}
	if _, err := s.transaction(func(entries map[string]CustomLibrarySource) error {
		entries[src.Name] = entry
		return nil
	}); err != nil {
		return CustomLibrarySource{}, err
	}
	s.notifyChanged()
	return entry, nil
}

// Remove deletes a configured source and its cached/generated files.
func (s *CustomLibrarySources) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if _, err := s.transaction(func(entries map[string]CustomLibrarySource) error {
		if _, ok := entries[name]; !ok {
			return fmt.Errorf("unknown custom library source %q", name)
		}
		delete(entries, name)
		return nil
	}); err != nil {
		return err
	}
	if err := s.syncer().RemoveSource(name); err != nil {
		s.logf("custom_library status=warn action=remove_cache name=%s err=%q", name, err)
	}
	s.notifyChanged()
	return nil
}

// SyncAll re-syncs every configured source. A source whose sync fails keeps
// its previously generated catalog and records the error for the UI; it is
// never removed by a failed background sync.
func (s *CustomLibrarySources) SyncAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.List()
	if err != nil {
		s.logf("custom_library status=error action=list err=%q", err)
		return
	}

	changed := false
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		result, syncErr := s.syncer().Sync(ctx, entry.Source)
		_, txErr := s.transaction(func(entries map[string]CustomLibrarySource) error {
			current, ok := entries[entry.Name]
			if !ok {
				return nil // removed concurrently
			}
			if syncErr != nil {
				current.LastError = syncErr.Error()
			} else {
				now := s.now()
				current.LastError = ""
				current.CommitSHA = result.CommitSHA
				current.TaskCount = result.TaskCount
				current.SyncedAt = &now
				if result.Changed {
					changed = true
				}
			}
			entries[entry.Name] = current
			return nil
		})
		if txErr != nil {
			s.logf("custom_library status=error action=sync name=%s err=%q", entry.Name, txErr)
		}
		if syncErr != nil {
			s.logf("custom_library status=warn action=sync name=%s err=%q", entry.Name, syncErr)
		}
	}
	if changed {
		s.notifyChanged()
	}
}

// Run syncs every configured source immediately, then again on every tick
// (hourly in production) until ctx is done.
func (s *CustomLibrarySources) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	s.SyncAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncAll(ctx)
		}
	}
}
