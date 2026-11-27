package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/stringsx"
)

// CheckTimings learns how long check waits take per repository and phase.
// Share one instance across the daemon's task attempts.
type CheckTimings struct {
	mu       sync.Mutex
	averages map[string]time.Duration
}

func (t *CheckTimings) average(key string) time.Duration {
	if t == nil || key == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.averages[key]
}

func (t *CheckTimings) record(key string, elapsed time.Duration) {
	if t == nil || key == "" || elapsed <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.averages == nil {
		t.averages = make(map[string]time.Duration)
	}
	previous := t.averages[key]
	if previous == 0 {
		if len(t.averages) >= 512 {
			for existing := range t.averages {
				delete(t.averages, existing)
				break
			}
		}
		t.averages[key] = elapsed
	} else {
		t.averages[key] = previous + (elapsed-previous)/4
	}
}

func checkPollDelay(expected, elapsed time.Duration) time.Duration {
	const fallback = prChecksWatchIntervalSeconds * time.Second
	if expected == 0 {
		return fallback
	}
	// Poll less often early in a long build and check more closely near its
	// expected completion. Back off again for builds that overrun the estimate.
	remaining := expected - elapsed
	if remaining < -fallback {
		return fallback
	}
	return min(45*time.Second, max(5*time.Second, remaining/2))
}

type checkPoll struct {
	stage, key string
	started    time.Time
	waited     bool
}

func newCheckPoll(stage, prURL string) checkPoll {
	target := strings.ToLower(githubutil.PullRequestRepository(prURL))
	var key string
	if target != "" {
		key = target + "/" + stage
	}
	return checkPoll{stage: stage, key: key, started: time.Now()}
}

func (p checkPoll) delay(h Harness) time.Duration {
	return checkPollDelay(h.CheckTimings.average(p.key), time.Since(p.started))
}

func (p checkPoll) finish(h Harness, learn bool) {
	elapsed := time.Since(p.started)
	prediction := h.CheckTimings.average(p.key)
	if learn {
		h.CheckTimings.record(p.key, elapsed)
	}
	h.logf("stage=%s status=ok action=github_checks_finished elapsed_ms=%d estimated_ms=%d", p.stage, elapsed.Milliseconds(), prediction.Milliseconds())
}

func (p checkPoll) report(h Harness, action, head string, pending []string, refreshed time.Time, delay time.Duration) {
	var refresh string
	if !refreshed.IsZero() {
		refresh = refreshed.UTC().Format(time.RFC3339)
	}
	h.logf("stage=%s status=waiting action=%s reviewed_head=%q pending_checks=%q next_poll_s=%d last_github_refresh=%q",
		p.stage, action, head, strings.Join(pending, ", "), int(delay.Seconds()), refresh)
}

func (h Harness) suspendWorkspace() {
	if h.ReleaseWorkspace != nil {
		h.ReleaseWorkspace()
	}
}

func (h Harness) requireWorkspace(ctx context.Context) error {
	if h.AcquireWorkspace == nil {
		return nil
	}
	if err := h.AcquireWorkspace(ctx); err != nil {
		return fmt.Errorf("task acquire: %w", err)
	}
	return nil
}

// Check a retained PR from outside its checkout before disk admission. This
// defers work while CI is pending or has yet to report. The regular path verifies
// branches, the checked-out head, and all required gates before proceeding.
func (h Harness) waitForResumedChecks(ctx context.Context, prURL string) (merged bool, err error) {
	poll := newCheckPoll("checks", prURL)
	timeout := h.prChecksWatchTimeout()
	deadline := poll.started.Add(timeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadlineCause(ctx, deadline, ErrChecksUnavailable)
		defer cancel()
		defer func() {
			if err != nil && errors.Is(context.Cause(ctx), ErrChecksUnavailable) {
				err = ErrChecksUnavailable
			}
		}()
	}
	pending := false
	var policyBase string
	var required []string
	for {
		if timeout > 0 && time.Until(deadline) <= 0 && ctx.Err() == nil {
			return false, ErrChecksUnavailable
		}
		res, err := h.runCheckQuery(ctx, "checks", prStateViewCommand("", prURL, "statusCheckRollup"), checkQueryTimeout)
		if err != nil {
			return false, commandErrorWithDetails("refresh resumed PR checks", err, res, maxGitErrorDetailChars)
		}
		var pr struct {
			prStateView
			StatusCheckRollup []githubutil.StatusCheck
		}
		if err := json.Unmarshal([]byte(res.Stdout), &pr); err != nil {
			return false, fmt.Errorf("%w: decode resumed PR checks: %w", ErrChecksUnavailable, err)
		}
		if pullRequestStateIsMerged(pr.prStateView) {
			return true, nil
		}
		if pr.State == "" {
			return false, fmt.Errorf("%w: resumed pull request state was missing", ErrChecksUnavailable)
		}
		if !pullRequestStateIsOpen(pr.prStateView) {
			return false, nil
		}
		visible := make(map[string]bool, len(pr.StatusCheckRollup))
		var names []string
		for _, check := range pr.StatusCheckRollup {
			name := stringsx.FirstNonEmpty(check.Name, check.Context)
			if name == "" || check.Name == githubutil.ReviewCheckName || check.Context == githubutil.ReviewCheckName {
				continue
			}
			visible[name] = true
			if passed, failed := githubutil.CIStatus([]githubutil.StatusCheck{check}); !passed && !failed {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			if pr.BaseRefName == "" || pr.HeadRefName == "" || pr.HeadRefOID == "" {
				return false, fmt.Errorf("%w: resumed pull request branches or head were missing", ErrChecksUnavailable)
			}
			repo := repoWorkspace{PRURL: prURL, BaseBranch: pr.BaseRefName, Branch: pr.HeadRefName}
			if policyBase != pr.BaseRefName {
				required, err = h.requiredCIStatusChecks(ctx, repo, false)
				if err != nil {
					return false, err
				}
				policyBase = pr.BaseRefName
			}
			for _, name := range required {
				if !visible[name] {
					names = append(names, name)
				}
			}
			settled := len(visible) > 0 && len(names) == 0
			if len(visible) == 0 {
				bucket, summary, err := h.readWorkflowDispatchResult(ctx, repo, pr.HeadRefOID)
				if err != nil {
					return false, err
				}
				settled = emptyCheckRollupSettled(required, bucket)
				if bucket == "pending" {
					workflow, _, _ := strings.Cut(summary, "\t")
					names = append(names, workflow)
				}
			}
			if settled {
				if pending {
					poll.finish(h, false)
				}
				return false, nil
			}
		}
		if timeout == 0 {
			return false, ErrChecksUnavailable
		}
		pending = true
		slices.Sort(names)
		deadline, _ := ctx.Deadline()
		delay := min(time.Until(deadline), poll.delay(h))
		poll.report(h, "wait_for_github_checks", "", slices.Compact(names), time.Now(), delay)
		if err := h.Sleep(ctx, delay); err != nil {
			return false, err
		}
	}
}

// A dispatch failure is actionable on its own. A successful dispatch cannot
// supply missing required PR check results.
func emptyCheckRollupSettled(required []string, dispatchBucket string) bool {
	return dispatchBucket == "fail" || (len(required) == 0 && dispatchBucket != "pending")
}

func checkWatchPending(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == 8
}
