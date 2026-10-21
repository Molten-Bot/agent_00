package localruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

const MaxLibraryScheduleHours = 168

// LibraryScheduleInput stores selection options, not an expanded library prompt.
// Resolve the current library definition for each occurrence.
type LibraryScheduleInput struct {
	LibraryTaskName string   `json:"libraryTaskName"`
	Repo            string   `json:"repo"`
	IntervalHours   int      `json:"intervalHours"`
	BaseBranch      string   `json:"baseBranch,omitempty"`
	TargetSubdir    string   `json:"targetSubdir,omitempty"`
	Reviewers       []string `json:"reviewers,omitempty"`
}

type LibrarySchedule struct {
	LibraryScheduleInput
	Key           string     `json:"key"`
	NextRunAt     time.Time  `json:"nextRunAt"`
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastRequestID string     `json:"lastRequestId,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

var scheduleNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var scheduleRepoPattern = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)

// LibraryScheduleKey treats GitHub SSH/HTTPS URLs and owner/repo as one repo.
func LibraryScheduleKey(name, repo string) (string, error) {
	name = strings.TrimSpace(name)
	if !scheduleNamePattern.MatchString(name) {
		return "", fmt.Errorf("library task name must be a task slug")
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	if strings.HasPrefix(repo, "git@github.com:") {
		repo = strings.TrimPrefix(repo, "git@github.com:")
	} else if strings.Contains(repo, "://") {
		u, err := url.Parse(repo)
		if err != nil || u.Host != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" || (u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git")) {
			return "", fmt.Errorf("repository must be a GitHub repository URL or owner/repo")
		}
		repo = strings.TrimPrefix(u.Path, "/")
	}
	repo = strings.TrimPrefix(repo, "github.com/")
	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	if !scheduleRepoPattern.MatchString(repo) {
		return "", fmt.Errorf("repository must be a GitHub repository URL or owner/repo")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("invalid repository")
		}
	}
	return name + "@" + repo, nil
}

func (s LibraryScheduleInput) RunConfig() (config.Config, error) {
	payload, err := json.Marshal(map[string]any{"libraryTaskName": s.LibraryTaskName, "repos": []string{s.Repo}, "baseBranch": s.BaseBranch, "targetSubdir": s.TargetSubdir, "reviewers": s.Reviewers})
	if err != nil {
		return config.Config{}, err
	}
	return ParseRunConfigJSON(payload)
}

// LibrarySchedules serializes edits and occurrence admission. Config transactions
// share the runtime settings lock so saving preferences cannot discard schedules.
type LibrarySchedules struct {
	Path   string
	Init   InitConfig
	Now    func() time.Time
	Submit func(context.Context, config.Config) (string, error)
	Active func(LibrarySchedule) bool
	Logf   func(string, ...any)
	mu     sync.Mutex
}

func (s *LibrarySchedules) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *LibrarySchedules) path() string {
	if strings.TrimSpace(s.Path) == "" {
		return defaultRuntimeConfigPath()
	}
	return s.Path
}

func schedulesFromDoc(doc map[string]any) (map[string]LibrarySchedule, error) {
	result := map[string]LibrarySchedule{}
	raw, ok := doc["library_schedules"]
	if !ok {
		return result, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("read library schedules: %w", err)
	}
	if result == nil {
		result = map[string]LibrarySchedule{}
	}
	for key, entry := range result {
		expected, err := LibraryScheduleKey(entry.LibraryTaskName, entry.Repo)
		if err != nil || key != expected || entry.Key != key || entry.IntervalHours < 1 || entry.IntervalHours > MaxLibraryScheduleHours || entry.NextRunAt.IsZero() {
			return nil, fmt.Errorf("invalid persisted library schedule %q", key)
		}
	}
	return result, nil
}

func (s *LibrarySchedules) transaction(change func(map[string]LibrarySchedule) error) ([]LibrarySchedule, error) {
	runtimeConfigMu.Lock()
	defer runtimeConfigMu.Unlock()
	doc, err := loadRuntimeConfigDoc(s.path(), s.Init)
	if err != nil {
		return nil, err
	}
	schedules, err := schedulesFromDoc(doc)
	if err != nil {
		return nil, err
	}
	if change != nil {
		if err := change(schedules); err != nil {
			return nil, err
		}
		doc["library_schedules"] = schedules
		if err := writeRuntimeConfigDoc(s.path(), doc); err != nil {
			return nil, err
		}
	}
	result := make([]LibrarySchedule, 0, len(schedules))
	for _, entry := range schedules {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

func (s *LibrarySchedules) List() ([]LibrarySchedule, error) { return s.transaction(nil) }

func (s *LibrarySchedules) Save(input LibraryScheduleInput) (LibrarySchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := LibraryScheduleKey(input.LibraryTaskName, input.Repo)
	if err != nil {
		return LibrarySchedule{}, err
	}
	if input.IntervalHours < 1 || input.IntervalHours > MaxLibraryScheduleHours {
		return LibrarySchedule{}, fmt.Errorf("intervalHours must be between 1 and %d", MaxLibraryScheduleHours)
	}
	input.LibraryTaskName = strings.TrimSpace(input.LibraryTaskName)
	input.Repo = "https://github.com/" + strings.SplitN(key, "@", 2)[1] + ".git"
	if _, err := input.RunConfig(); err != nil {
		return LibrarySchedule{}, err
	}
	entry := LibrarySchedule{LibraryScheduleInput: input, Key: key, NextRunAt: s.now().Add(time.Duration(input.IntervalHours) * time.Hour)}
	_, err = s.transaction(func(entries map[string]LibrarySchedule) error {
		if old, ok := entries[key]; ok {
			entry.LastRunAt = old.LastRunAt
			entry.LastRequestID = old.LastRequestID
		}
		entries[key] = entry
		return nil
	})
	return entry, err
}

func (s *LibrarySchedules) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("schedule key is required")
	}
	_, err := s.transaction(func(entries map[string]LibrarySchedule) error { delete(entries, key); return nil })
	return err
}

// RunDue reserves the next time before submitting, preventing replay on restart.
// A failed submission waits until the next interval; errors remain visible in UI.
func (s *LibrarySchedules) RunDue(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Submit == nil {
		return nil
	}
	entries, err := s.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := s.now()
		if entry.NextRunAt.After(now) {
			continue
		}
		if s.Active != nil && s.Active(entry) {
			continue
		}
		entry.NextRunAt = now.Add(time.Duration(entry.IntervalHours) * time.Hour)
		_, err := s.transaction(func(entries map[string]LibrarySchedule) error { entries[entry.Key] = entry; return nil })
		if err != nil {
			return err
		}
		cfg, runErr := entry.RunConfig()
		id := ""
		if runErr == nil {
			id, runErr = s.Submit(ctx, cfg)
		}
		entry.LastError = ""
		if runErr != nil {
			entry.LastError = runErr.Error()
		} else {
			entry.LastRunAt = &now
			entry.LastRequestID = id
		}
		if s.Logf != nil {
			status := "submitted"
			if runErr != nil {
				status = "error"
			}
			s.Logf("library.schedule key=%s request_id=%s status=%s", entry.Key, id, status)
		}
		_, err = s.transaction(func(entries map[string]LibrarySchedule) error { entries[entry.Key] = entry; return nil })
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *LibrarySchedules) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if err := s.RunDue(ctx); err != nil && ctx.Err() == nil && s.Logf != nil {
			s.Logf("library.schedule status=error err=%q", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
