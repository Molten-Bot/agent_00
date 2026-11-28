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

// CheckTimings learns how long CI waits take per repository.
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
	stage, key          string
	started             time.Time
	waited              bool
	refreshMergeability func(context.Context) error
}

func newCheckPoll(stage, prURL string) checkPoll {
	target := strings.ToLower(githubutil.PullRequestRepository(prURL))
	var key string
	if target != "" {
		// Draft verification waits on the same CI, but starts partway through it.
		key = target + "/checks"
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
	resumeDeadline := poll.started.Add(timeout)
	resumeCtx, pollCtx := ctx, ctx
	var reconcileAt time.Time
	var incompleteRollupHead prStateView
	var observedHead string
	observedPending := make(map[string]bool)
	if timeout > 0 {
		var cancel context.CancelFunc
		resumeCtx, cancel = context.WithDeadlineCause(ctx, resumeDeadline, ErrChecksUnavailable)
		defer cancel()
		// Leave time for paginated reconciliation when the CLI rollup is incomplete.
		deadline, _ := resumeCtx.Deadline()
		reserve := min(checkQueryTimeout, deadline.Sub(poll.started)/5)
		reconcileAt = deadline.Add(-reserve)
		pollCtx, cancel = context.WithDeadlineCause(resumeCtx, reconcileAt, ErrChecksUnavailable)
		defer cancel()
	}
	ctx = resumeCtx
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), ErrChecksUnavailable) {
			err = ErrChecksUnavailable
		}
		if errors.Is(err, ErrChecksUnavailable) && timeout > 0 && incompleteRollupHead.HeadRefOID != "" &&
			!time.Now().Before(reconcileAt) && resumeCtx.Err() == nil {
			merged, err = h.reconcileResumedChecks(resumeCtx, prURL, incompleteRollupHead, observedPending)
			if err != nil && errors.Is(context.Cause(resumeCtx), ErrChecksUnavailable) {
				err = errors.Join(ErrChecksUnavailable, err)
			}
		}
	}()
	var policyBase string
	var required []string
	unknownMergeability := 0
	for {
		if timeout > 0 && (incompleteRollupHead.HeadRefOID != "" && !time.Now().Before(reconcileAt) ||
			!time.Now().Before(resumeDeadline)) {
			return false, ErrChecksUnavailable
		}
		res, err := h.runCheckQuery(ctx, "checks", prStateViewCommand("", prURL, "statusCheckRollup", "id", "mergeable"), checkQueryTimeout)
		if err != nil {
			return false, commandErrorWithDetails("refresh resumed PR checks", err, res, maxGitErrorDetailChars)
		}
		var pr struct {
			prMergeability
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
		if pr.BaseRefName == "" || pr.HeadRefName == "" || pr.HeadRefOID == "" {
			incompleteRollupHead = prStateView{}
			return false, fmt.Errorf("%w: resumed pull request branches or head were missing", ErrChecksUnavailable)
		}
		if incompleteRollupHead.HeadRefOID != pr.HeadRefOID ||
			incompleteRollupHead.BaseRefName != pr.BaseRefName || incompleteRollupHead.HeadRefName != pr.HeadRefName {
			incompleteRollupHead = prStateView{}
			ctx = resumeCtx
		}
		if observedHead != pr.HeadRefOID {
			clear(observedPending)
			observedHead = pr.HeadRefOID
		}
		switch pr.Mergeable {
		case "CONFLICTING":
			h.logf("stage=merge_conflict status=start action=detect pr_url=%s head=%s base_branch=%s", prURL, pr.HeadRefOID, pr.BaseRefName)
			// Yield to workspace admission. The repair path reports progress
			// after admission, when it can persist the step and start repair.
			return false, nil
		case "UNKNOWN":
			incompleteRollupHead = prStateView{}
			ctx = resumeCtx
			unknownMergeability++
			if unknownMergeability >= maxMergeabilityChecks || timeout == 0 {
				return false, fmt.Errorf("%w: pull request mergeability is unknown", ErrChecksUnavailable)
			}
			if err := h.Sleep(ctx, mergeabilityRetryDelay); err != nil {
				return false, err
			}
			continue
		}
		unknownMergeability = 0
		visible := make(map[string]bool, len(pr.StatusCheckRollup))
		var names []string
		for _, check := range pr.StatusCheckRollup {
			name := stringsx.FirstNonEmpty(check.Name, check.Context)
			if name == "" || check.Name == githubutil.ReviewCheckName || check.Context == githubutil.ReviewCheckName {
				continue
			}
			visible[name] = true
			if passed, failed := githubutil.CIStatus([]githubutil.StatusCheck{check}); !passed && !failed {
				observedPending[name] = true
				names = append(names, name)
			}
		}
		if len(names) == 0 {
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
			if len(names) == 0 && len(required) > 0 {
				// The CLI rollup only carries names. GitHub's required flag
				// distinguishes a required result from a same-name optional one.
				checks, err := h.readPRCheckRollup(ctx, repo, pr.ID, pr.HeadRefOID)
				if errors.Is(err, githubutil.ErrReviewInterrupted) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				missing, pending := resumedRollupPending(checks, required, observedPending)
				names = append(missing, pending...)
			} else {
				for name := range observedPending {
					if !visible[name] {
						names = append(names, name)
					}
				}
			}
			incompleteRollupHead = prStateView{}
			ctx = resumeCtx
			if len(names) > 0 {
				incompleteRollupHead = pr.prStateView
				ctx = pollCtx
			}
			bucket, summary, err := h.readWorkflowDispatchResult(ctx, repo, pr.HeadRefOID)
			if err != nil {
				return false, err
			}
			if (len(names) == 0 && bucket != "pending") || (len(visible) == 0 && bucket == "fail") {
				// Let the normal check path handle a dispatch failure with no checks.
				if poll.waited {
					poll.finish(h, false)
				}
				return false, nil
			}
			if bucket == "pending" {
				workflow, _, _ := strings.Cut(summary, "\t")
				names = append(names, workflow)
			}
		} else {
			incompleteRollupHead = prStateView{}
			ctx = resumeCtx
		}
		if timeout == 0 {
			return false, ErrChecksUnavailable
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		poll.waited = true
		slices.Sort(names)
		deadline, _ := ctx.Deadline()
		delay := min(time.Until(deadline), poll.delay(h))
		poll.report(h, "wait_for_github_checks", "", slices.Compact(names), time.Now(), delay)
		if err := h.Sleep(ctx, delay); err != nil {
			return false, err
		}
	}
}

// After the propagation wait, reconcile missing results with the paginated PR
// rollup and current branch policy before deciding whether to retry.
func (h Harness) reconcileResumedChecks(ctx context.Context, prURL string, expected prStateView, observedPending map[string]bool) (bool, error) {
	repo := repoWorkspace{PRURL: prURL, BaseBranch: expected.BaseRefName, Branch: expected.HeadRefName}
	state, err := h.readCheckPRState(ctx, prStateViewCommand("", prURL, "id"))
	if err != nil {
		return false, err
	}
	if err := checkPRMatches(state, repo, expected.HeadRefOID); err != nil {
		if errors.Is(err, githubutil.ErrReviewInterrupted) {
			return true, nil
		}
		return false, err
	}
	bucket, summary, err := h.readWorkflowDispatchResult(ctx, repo, expected.HeadRefOID)
	if err != nil {
		return false, err
	}
	if bucket == "pending" {
		return false, ErrChecksUnavailable
	}
	required, err := h.requiredCIStatusChecks(ctx, repo, false)
	if err != nil {
		return false, err
	}
	checks, err := h.readPRCheckRollup(ctx, repo, state.ID, expected.HeadRefOID)
	if errors.Is(err, githubutil.ErrReviewInterrupted) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	missing, pending := resumedRollupPending(checks, required, observedPending)
	if len(pending) > 0 || (len(missing) > 0 && bucket == "") {
		return false, ErrChecksUnavailable
	}
	if len(missing) > 0 {
		workflow, _, _ := strings.Cut(summary, "\t")
		slices.Sort(missing)
		return false, fmt.Errorf("workflow dispatch %q completed, but required checks are missing on current head %s: %s. Verify the workflow publishes these check names on the pull request head, then rerun CI and retry", workflow, expected.HeadRefOID, strings.Join(slices.Compact(missing), ", "))
	}
	return false, nil
}

func resumedRollupPending(checks []prCheckRollupEntry, required []string, observedPending map[string]bool) (missing, pending []string) {
	reported := make(map[string]bool, len(checks))
	for _, check := range checks {
		if check.Name == githubutil.ReviewCheckName || check.Context == githubutil.ReviewCheckName {
			continue
		}
		name := stringsx.FirstNonEmpty(check.Name, check.Context)
		if passed, failed := githubutil.CIStatus([]githubutil.StatusCheck{check.StatusCheck}); !passed && !failed {
			observedPending[name] = true
			pending = append(pending, name)
		}
		reported[name] = reported[name] || check.IsRequired
	}
	for _, name := range required {
		if !reported[name] {
			missing = append(missing, name)
		}
	}
	for name := range observedPending {
		if _, ok := reported[name]; !ok {
			pending = append(pending, name)
		}
	}
	return missing, pending
}

func checkWatchPending(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == 8
}
