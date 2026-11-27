package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

const defaultPRMergePollInterval = time.Minute
const prMonitorTitleLookupTimeout = 2 * time.Second
const prMonitorStateLookupTimeout = 30 * time.Second

// PRMergeMonitor watches task pull requests and marks merged tasks as done.
type PRMergeMonitor struct {
	Runner            execx.Runner
	Broker            *Broker
	Logf              func(string, ...any)
	CleanupTask       func(context.Context, string) error
	PollInterval      time.Duration
	Cooldown          *githubutil.Cooldown
	MemoryPressure    func() bool
	RepositoryAllowed func(context.Context, string) (bool, error)

	mu       sync.Mutex
	inFlight map[string]struct{}
	merged   map[string]struct{}
}

type prViewState struct {
	State    string `json:"state"`
	MergedAt string `json:"mergedAt"`
	Title    string `json:"title"`
}

// Run polls tracked PRs until ctx is canceled.
func (m *PRMergeMonitor) Run(ctx context.Context) error {
	if m == nil || m.Broker == nil || m.Runner == nil {
		return nil
	}
	if m.Logf == nil {
		m.Logf = func(string, ...any) {}
	}
	if m.PollInterval <= 0 {
		m.PollInterval = defaultPRMergePollInterval
	}

	ticker := time.NewTicker(m.PollInterval)
	defer ticker.Stop()

	m.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *PRMergeMonitor) pollOnce(ctx context.Context) {
	if (m.MemoryPressure != nil && m.MemoryPressure()) || !m.Cooldown.RetryAt().IsZero() {
		return
	}
	ctx = githubutil.WithCaller(ctx, "pr_monitor")
	tasks := m.Broker.TaskList()
	active := make(map[string]struct{}, len(tasks))
	reads := map[string]func() (prViewState, error){}
	for _, task := range tasks {
		active[task.RequestID] = struct{}{}
		if !shouldMonitorTaskPR(task) {
			continue
		}
		if !m.beginCheck(task.RequestID) {
			continue
		}
		read := reads[task.PRURL]
		if read == nil {
			read = sync.OnceValues(func() (prViewState, error) {
				if !m.allowed(ctx, task.PRURL) {
					return prViewState{}, nil
				}
				return m.prState(ctx, task.PRURL)
			})
			reads[task.PRURL] = read
		}
		go func(task Task) {
			defer m.endCheck(task.RequestID)
			state, err := read()
			m.applyPRState(ctx, task, state, err)
		}(task)
	}
	m.forgetMissingTasks(active)
}

func shouldMonitorTaskPR(task Task) bool {
	if strings.TrimSpace(task.PRURL) == "" {
		return false
	}
	switch strings.TrimSpace(task.Status) {
	case "completed", "no_changes", "merged":
		// A workflow can mark a task merged before the monitor records its release.
		return true
	default:
		return false
	}
}

func (m *PRMergeMonitor) beginCheck(requestID string) bool {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight == nil {
		m.inFlight = map[string]struct{}{}
	}
	if m.merged == nil {
		m.merged = map[string]struct{}{}
	}
	if _, exists := m.inFlight[requestID]; exists {
		return false
	}
	if _, exists := m.merged[requestID]; exists {
		return false
	}
	m.inFlight[requestID] = struct{}{}
	return true
}

func (m *PRMergeMonitor) endCheck(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inFlight, strings.TrimSpace(requestID))
}

func (m *PRMergeMonitor) markMerged(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.merged == nil {
		m.merged = map[string]struct{}{}
	}
	m.merged[requestID] = struct{}{}
}

func (m *PRMergeMonitor) forgetMissingTasks(active map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for requestID := range m.merged {
		if _, ok := active[requestID]; ok {
			continue
		}
		delete(m.merged, requestID)
	}
}

func (m *PRMergeMonitor) allowed(ctx context.Context, prURL string) bool {
	if m.RepositoryAllowed == nil {
		return true
	}
	repo := githubutil.PullRequestRepository(prURL)
	if repo == "" {
		return false
	}
	allowed, err := m.RepositoryAllowed(ctx, repo)
	return err == nil && allowed
}

func (m *PRMergeMonitor) applyPRState(ctx context.Context, task Task, state prViewState, err error) {
	if err != nil {
		m.Logf("localruntime.ui status=warn event=pr_monitor request_id=%s pr_url=%s err=%q", task.RequestID, task.PRURL, err)
		return
	}
	if state.State == "" && state.MergedAt == "" && state.Title == "" {
		return
	}
	if !isReviewTask(task.Source, task.Workflow) {
		if task.PRTitleURL == "" || task.PRTitleURL == task.PRURL {
			m.Broker.recordTaskPRTitle(task.RequestID, task.PRURL, state.Title, taskPRTitleMonitored)
		} else if (m.MemoryPressure == nil || !m.MemoryPressure()) && m.allowed(ctx, task.PRTitleURL) {
			titleCtx, cancel := context.WithTimeout(ctx, prMonitorTitleLookupTimeout)
			result, err := m.Cooldown.Run(titleCtx, m.Runner, execx.Command{
				Name: "gh",
				Args: []string{"pr", "view", task.PRTitleURL, "--json", "title", "--jq", ".title"},
			}, false)
			cancel()
			if err != nil {
				m.Logf("localruntime.ui status=warn event=pr_monitor_title request_id=%s pr_url=%s err=%q", task.RequestID, task.PRTitleURL, err)
			} else {
				m.Broker.recordTaskPRTitle(task.RequestID, task.PRTitleURL, result.Stdout, taskPRTitleMonitored)
			}
		}
	}
	if !state.Merged() {
		return
	}
	m.Broker.RecordReleaseFromTask(task, state.MergedAt)
	if err := m.Broker.MarkTaskPRMerged(task.RequestID, state.MergedAt); err != nil {
		switch {
		case err == ErrTaskNotFound:
			m.markMerged(task.RequestID)
			return
		default:
			m.Logf("localruntime.ui status=warn event=pr_monitor_mark_merged request_id=%s pr_url=%s err=%q", task.RequestID, task.PRURL, err)
			return
		}
	}
	m.Broker.DropTaskRunConfig(task.RequestID)
	if m.CleanupTask != nil {
		if err := m.CleanupTask(ctx, task.RequestID); err != nil {
			m.Logf("localruntime.ui status=warn event=pr_monitor_cleanup request_id=%s pr_url=%s err=%q", task.RequestID, task.PRURL, err)
		}
	}
	m.markMerged(task.RequestID)
	m.Logf("localruntime.ui status=ok event=pr_merged request_id=%s pr_url=%s", task.RequestID, task.PRURL)
}

func (m *PRMergeMonitor) prState(ctx context.Context, prURL string) (prViewState, error) {
	prURL = strings.TrimSpace(prURL)
	if prURL == "" {
		return prViewState{}, fmt.Errorf("pull request url is required")
	}
	args := []string{"pr", "view", githubutil.PullRequestSelector(prURL), "--json", "state,mergedAt,title"}
	if repo := githubutil.PullRequestRepository(prURL); repo != "" {
		args = append(args, "--repo", repo)
	}
	ctx, cancel := context.WithTimeout(ctx, prMonitorStateLookupTimeout)
	defer cancel()
	res, err := m.Cooldown.Run(ctx, m.Runner, execx.Command{
		Name: "gh",
		Args: args,
	}, false)
	if err != nil {
		return prViewState{}, err
	}
	var state prViewState
	if decodeErr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &state); decodeErr != nil {
		return prViewState{}, fmt.Errorf("decode gh pr view response: %w", decodeErr)
	}
	return state, nil
}

func (s prViewState) Merged() bool {
	return strings.EqualFold(strings.TrimSpace(s.State), "merged") || strings.TrimSpace(s.MergedAt) != ""
}
