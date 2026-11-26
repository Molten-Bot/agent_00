package localruntime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type IssueRecoveryConfig struct {
	MaxAttempts        int `json:"max_attempts,omitempty"`
	StepTimeoutSeconds int `json:"step_timeout_seconds,omitempty"`
	RetryDelaySeconds  int `json:"retry_delay_seconds,omitempty"`
	MaxBackoffSeconds  int `json:"max_backoff_seconds,omitempty"`
}

func (c IssueRecoveryConfig) Defaults() IssueRecoveryConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.StepTimeoutSeconds <= 0 {
		c.StepTimeoutSeconds = 7200
	}
	if c.RetryDelaySeconds <= 0 {
		c.RetryDelaySeconds = 60
	}
	if c.MaxBackoffSeconds <= 0 {
		c.MaxBackoffSeconds = 3600
	}
	return c
}

func IssueExecutionKey(issue *config.GitHubIssueConfig) string {
	return fmt.Sprintf("issue-%x", sha256.Sum256([]byte(fmt.Sprintf("%s#%d", strings.ToLower(issue.Repository), issue.Number))))
}

// TryLock is nonblocking. The open descriptor, not a PID or a timestamp, owns
// the lock. Never unlink lock files: a surviving child may still hold the inode.
func (s *IssueClaimStore) TryLock(key string) (*os.File, error) {
	if s == nil || s.Root == "" || strings.ContainsAny(key, "/\\.") || key == "" {
		return nil, fmt.Errorf("invalid issue lock")
	}
	root := filepath.Join(s.Root, "locks")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, key), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ok, err := tryIssueLock(f)
	if !ok || err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// IssueRunner keeps the worker lock in every subprocess, including agents.
// It also bounds pipe draining after process exit in OSRunner.
type IssueRunner struct {
	Runner   execx.Runner
	Lock     *os.File
	Check    func(context.Context) error
	Store    *IssueClaimStore
	RecordID string
}

func (r IssueRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	if err := r.check(ctx, cmd); err != nil {
		return execx.Result{}, err
	}
	var err error
	cmd, err = r.track(cmd)
	if err != nil {
		return execx.Result{}, err
	}
	cmd.InheritedFiles = append(cmd.InheritedFiles, r.Lock)
	return r.Runner.Run(ctx, cmd)
}
func (r IssueRunner) RunStream(ctx context.Context, cmd execx.Command, line execx.StreamLineHandler) (execx.Result, error) {
	if err := r.check(ctx, cmd); err != nil {
		return execx.Result{}, err
	}
	var trackErr error
	cmd, trackErr = r.track(cmd)
	if trackErr != nil {
		return execx.Result{}, trackErr
	}
	cmd.InheritedFiles = append(cmd.InheritedFiles, r.Lock)
	if stream, ok := r.Runner.(execx.StreamRunner); ok {
		return stream.RunStream(ctx, cmd, line)
	}
	res, err := r.Runner.Run(ctx, cmd)
	if res.Stdout != "" {
		line("stdout", res.Stdout)
	}
	if res.Stderr != "" {
		line("stderr", res.Stderr)
	}
	return res, err
}

func (r IssueRunner) track(cmd execx.Command) (execx.Command, error) {
	if r.Store == nil {
		return cmd, nil
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err := r.Store.Update(r.RecordID, func(record *IssueClaimRecord) error {
		record.ProcessUncertain = true
		record.ProcessBootID = strings.TrimSpace(string(boot))
		return nil
	}); err != nil {
		return cmd, err
	}
	cmd.Started = func(pid int) error {
		return r.Store.Update(r.RecordID, func(record *IssueClaimRecord) error {
			record.ProcessGroups = append(record.ProcessGroups, pid)
			record.ProcessUncertain = false
			return nil
		})
	}
	cmd.StartFailed = func() {
		_ = r.Store.Update(r.RecordID, func(record *IssueClaimRecord) error { record.ProcessUncertain = false; return nil })
	}
	cmd.Exited = func(pid int) {
		alive, err := processGroupAlive(pid)
		if err != nil || alive {
			return
		}
		// A failed update leaves conservative evidence of a possibly live group.
		_ = r.Store.Update(r.RecordID, func(record *IssueClaimRecord) error {
			record.ProcessGroups = slices.DeleteFunc(record.ProcessGroups, func(group int) bool { return group == pid })
			record.ProcessUncertain = false
			return nil
		})
	}
	return cmd, nil
}

// IssueProcessesStopped distinguishes proven termination from an orphaned
// process group or a crash between dispatch intent and process registration.
func IssueProcessesStopped(record IssueClaimRecord) (bool, error) {
	if !record.ProcessUncertain && len(record.ProcessGroups) == 0 {
		return true, nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err == nil && record.ProcessBootID != "" && strings.TrimSpace(string(boot)) != record.ProcessBootID {
		return true, nil
	}
	if record.ProcessUncertain {
		return false, fmt.Errorf("subprocess start was interrupted; termination requires explicit confirmation")
	}
	for _, pid := range record.ProcessGroups {
		alive, err := processGroupAlive(pid)
		if err != nil {
			return false, err
		}
		if alive {
			return false, nil
		}
	}
	return true, nil
}

func (r IssueRunner) check(ctx context.Context, cmd execx.Command) error {
	if r.Check == nil {
		return nil
	}
	for _, arg := range cmd.Args {
		if arg == "push" || arg == "create" || arg == "merge" || arg == "edit" || arg == "ready" {
			return r.Check(ctx)
		}
	}
	return nil
}

// StepWatchdog resets only on a different semantic workflow step. Output and
// heartbeats never reset it. A generation prevents a racing old timer from
// canceling a task that has already advanced.
type StepWatchdog struct {
	mu         sync.Mutex
	step       string
	generation uint64
	timer      *time.Timer
	timeout    time.Duration
	cancel     context.CancelCauseFunc
	closed     bool
}

func NewStepWatchdog(timeout time.Duration, cancel context.CancelCauseFunc) *StepWatchdog {
	return &StepWatchdog{timeout: timeout, cancel: cancel}
}
func (w *StepWatchdog) Advance(step string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.step == step {
		return false
	}
	w.step = step
	w.generation++
	generation := w.generation
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.timeout, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.closed && generation == w.generation {
			w.cancel(fmt.Errorf("workflow step %s exceeded %s", step, w.timeout))
		}
	})
	return true
}
func (w *StepWatchdog) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
}
