package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func issueExplicitRetryError(incident string) string {
	switch incident {
	case localruntime.IssueIncidentWorkspaceStorage:
		return "issue workspace storage unavailable; free space or restore a disk-backed mount, then select Retry"
	case localruntime.IssueIncidentUnmergedDependency:
		return failurefollowup.UnmergedDependencyReason + "; explicit retry required after merge"
	case localruntime.IssueIncidentOpenDependency:
		return failurefollowup.OpenDependencyReason + "; explicit retry required after resolution"
	case localruntime.IssueIncidentExternalRollout:
		return failurefollowup.ExternalRolloutPrerequisitesReason + "; explicit retry required after provisioning"
	case localruntime.IssueIncidentHumanAction:
		return failurefollowup.HumanActionRequiredReason + "; explicit retry required after the handoff"
	case localruntime.IssueIncidentExternalEvidence:
		return failurefollowup.ExternalEvidencePrerequisitesReason + "; explicit retry required after evidence and access are available"
	default:
		return ""
	}
}

func (s Server) WakeIssueRecovery() {
	select {
	case s.issueRecoveryWake <- struct{}{}:
	default:
	}
}

var errIssueRecoveryDeferred = errors.New("issue recovery deferred during memory pressure")

const issueAlreadyMergedError = "issue already has a merged PR"
const mergedUnverifiedIssueError = "pull request merged before draft verification passed"

// Retained claims share the GitHub quota with active delivery and review.
const retainedIssuePRPollInterval = 5 * time.Minute

func (s Server) runIssueRecovery(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		err := s.ReconcileIssueWork(ctx)
		if s.MaintainIssueWorkspaces != nil && ctx.Err() == nil {
			s.issueAutomation.admission.Lock()
			s.MaintainIssueWorkspaces(ctx)
			s.issueAutomation.admission.Unlock()
		}
		var status issueAutomationStatus
		var rate *githubutil.RateLimitError
		if err != nil && !errors.Is(err, errIssueRecoveryDeferred) && ctx.Err() == nil {
			status.addError(err)
			if errors.As(err, &rate) {
				s.logf("localruntime.ui status=waiting event=github_issue_recovery retry_at=%s", rate.RetryAt.UTC().Format(time.RFC3339))
			} else {
				s.logf("localruntime.ui status=warn event=github_issue_recovery err=%q", status.Error)
			}
		}
		s.issueAutomation.mu.Lock()
		if !errors.Is(err, errIssueRecoveryDeferred) {
			s.issueAutomation.status = status
		}
		s.issueAutomation.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.issueRecoveryWake:
		}
	}
}

func (s Server) admitDurableIssue(ctx context.Context, run config.Config, settings ReviewSettingsState, login string, submit func(context.Context, []byte) (string, error)) (string, error) {
	claim := &config.GitHubIssueClaim{ID: newIssueClaimID(), Branch: fmt.Sprintf("agent00-issue-%d", run.GitHubIssue.Number), BaseBranch: run.BaseBranch, ExclusionLabel: issueExclusionLabel(settings), Attempt: 1}
	run.GitHubIssue.Claim = claim
	record := localruntime.IssueClaimRecord{Version: 2, ID: claim.ID, InstanceID: s.instanceID(), Phase: "observed", State: "observed", Run: run}
	if record.InstanceID == "" {
		return "", fmt.Errorf("persistent instance identity is unavailable")
	}
	if err := s.IssueClaims.Save(record); err != nil {
		return "", err
	}
	prepared, err := s.prepareObservedIssue(ctx, record, settings, login)
	if err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	return s.resumeIssueAdmission(ctx, prepared, submit)
}

func (s Server) prepareObservedIssue(ctx context.Context, record localruntime.IssueClaimRecord, settings ReviewSettingsState, login string) (localruntime.IssueClaimRecord, error) {
	issue := record.Run.GitHubIssue
	fresh, err := s.fetchGitHubIssue(ctx, issue.Repository, issue.Number)
	if err != nil {
		return record, err
	}
	if reason := issueEligibilityReason(fresh, settings, login); reason != "" {
		if record.Run.Workflow != nil && reason == "claimed elsewhere or excluded" {
			if err := s.checkObservedIssueOwnership(ctx, record); err != nil {
				return record, err
			}
		}
		return record, fmt.Errorf("issue is ineligible: %s", reason)
	}
	target, err := s.issueClaimTarget(ctx, issue.Repository, issue.Number)
	if err != nil {
		return record, err
	}
	if target.Linked > 0 || target.ClosingPRs > 0 {
		if record.Run.Workflow != nil {
			if err := s.checkObservedIssueOwnership(ctx, record); err != nil {
				return record, err
			}
		}
		return record, fmt.Errorf("issue has an associated branch or closing PR; ownership requires inspection")
	}
	if target.BlockedBy > 0 {
		return record, fmt.Errorf("issue has open dependencies")
	}
	base := record.Run.BaseBranch
	if base == "" {
		base = target.BaseBranch
	}
	sha := target.SHA
	if base != target.BaseBranch {
		sha, err = s.issueBranchSHA(ctx, issue.Repository, base)
		if err != nil {
			return record, err
		}
	}
	record.Run.BaseBranch = base
	issue.Claim.BaseBranch = base
	setGitHubIssuePrompt(&record.Run, fresh)
	if err := githubassets.Validate(record.Run.Assets); err != nil {
		return record, err
	}
	record.PlanningStatus, err = s.prepareIssuePlanning(ctx, issue, target, false)
	if err != nil {
		return record, err
	}
	record.IssueNodeID = target.ID
	record.ReservationSHA = sha
	record.Phase = "reserving"
	record.State = "reserving"
	return record, s.IssueClaims.Save(record)
}

func (s Server) checkObservedIssueOwnership(ctx context.Context, record localruntime.IssueClaimRecord) error {
	issue := record.Run.GitHubIssue
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err == nil && remote.Claim.ID != "" && remote.Claim.ID != record.ID {
		return errWorkflowIssueRecovery
	}
	return err
}

const workflowIssueRecoveryIncident = "workflow_issue_recovery"

var errWorkflowIssueRecovery = errors.New("issue is reserved by a previous attempt; stop its worker on every instance, then use Recover / retry to resume through the workflow")

// An observation has captured workflow settings but has never owned a worker or ref.
func isUnadmittedWorkflowIssue(record localruntime.IssueClaimRecord) bool {
	return record.Version == 2 && record.Run.Workflow != nil && record.Run.GitHubIssue.Claim.Attempt == 1 && !record.RemoteRecorded &&
		!record.BranchReserved && record.RequestID == "" && record.WorkspaceDir == "" &&
		record.StartedAt.IsZero() && record.Outcome == "" &&
		(record.Phase == "observed" || record.Phase == "blocked")
}

func blockObservedWorkflowIssue(record *localruntime.IssueClaimRecord) {
	record.Phase = "blocked"
	record.State = "blocked"
	record.Incident = workflowIssueRecoveryIncident
	record.Error = errWorkflowIssueRecovery.Error()
	record.NextAttemptAt = time.Time{}
}

// Resume only effects whose unique remote identity matches this durable intent.
// Ownership tags precede linked-branch creation in protocol version two.
func (s Server) resumeIssueAdmission(ctx context.Context, record localruntime.IssueClaimRecord, submit func(context.Context, []byte) (string, error)) (string, error) {
	if err := s.IssueClaims.CheckOwner(record.InstanceID); err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	issue := record.Run.GitHubIssue
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	if remote.Claim.ID != record.ID {
		if remote.Claim.Attempt >= issue.Claim.Attempt {
			if record.Run.Workflow != nil && !record.RemoteRecorded && issue.Claim.Attempt == 1 {
				return "", s.blockIssueClaim(record, errWorkflowIssueRecovery)
			}
			return "", s.blockIssueClaim(record, fmt.Errorf("claim belongs to another attempt; explicit recovery required"))
		}
		if err := s.publishIssueAttempt(ctx, issue, record.ReservationSHA, record.InstanceID); err != nil {
			// An accepted mutation can lose its response. Confirm the exact tag.
			check, checkErr := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
			if checkErr != nil || check.Claim.ID != record.ID || check.InstanceID != record.InstanceID {
				return "", s.blockIssueClaim(record, errors.Join(err, checkErr))
			}
		}
	} else if remote.InstanceID != record.InstanceID {
		return "", s.blockIssueClaim(record, fmt.Errorf("claim owner mismatch"))
	}
	record.RemoteRecorded = true
	if err := s.IssueClaims.Save(record); err != nil {
		return "", err
	}
	if !record.BranchReserved {
		if err := s.reserveIssueBranch(ctx, record.IssueNodeID, issue.Claim.Branch, record.ReservationSHA); err != nil {
			target, checkErr := s.issueClaimTarget(ctx, issue.Repository, issue.Number)
			sha, shaErr := s.issueBranchSHA(ctx, issue.Repository, issue.Claim.Branch)
			if checkErr != nil || shaErr != nil || target.Linked != 1 || len(target.Branches) != 1 || target.Branches[0] != issue.Claim.Branch || sha != record.ReservationSHA {
				return "", s.blockIssueClaim(record, errors.Join(err, checkErr, shaErr))
			}
		}
		record.BranchReserved = true
		if err := s.IssueClaims.Save(record); err != nil {
			return "", err
		}
	}
	return s.finishIssueAdmission(ctx, record, submit)
}

// ReconcileIssueWork checks existing claims after startup and worker teardown. It does not rely on
// broker terminal status, labels, or elapsed time to establish worker liveness.
func (s Server) ReconcileIssueWork(ctx context.Context) error {
	if s.IssueRecovery == nil || s.IssueClaims == nil || s.issueAutomation == nil {
		return nil
	}
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()
	if s.MemoryPressure != nil && s.MemoryPressure() {
		return errIssueRecoveryDeferred
	}
	now := s.Broker.now()
	s.issueAutomation.mu.Lock()
	maps.DeleteFunc(s.issueAutomation.linkedPRPollAfter, func(_ string, next time.Time) bool {
		return !now.Before(next)
	})
	s.issueAutomation.mu.Unlock()
	ctx, cancel := context.WithTimeout(githubutil.WithCaller(ctx, "issue_recovery"), 30*time.Second)
	defer cancel()
	records, listErr := s.IssueClaims.List()
	latest := map[string]localruntime.IssueClaimRecord{}
	for _, record := range records {
		key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
		old, ok := latest[key]
		if !ok || record.Run.GitHubIssue.Claim.Attempt > old.Run.GitHubIssue.Claim.Attempt || (record.Run.GitHubIssue.Claim.Attempt == old.Run.GitHubIssue.Claim.Attempt && record.UpdatedAt.After(old.UpdatedAt)) {
			latest[key] = record
		}
	}
	var problems []error
	if listErr != nil {
		problems = append(problems, listErr)
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		defer s.Broker.notifyIssueTaskUpdate()
	}
	cursor, err := s.IssueClaims.ScanPage("@reconcile", 0)
	if err != nil {
		return errors.Join(listErr, err)
	}
	for offset := range keys {
		if s.MemoryPressure != nil && s.MemoryPressure() {
			break
		}
		index := (cursor - 1 + offset) % len(keys)
		record := latest[keys[index]]
		if ctx.Err() != nil {
			return errors.Join(append(problems, ctx.Err())...)
		}
		if record.Version != 2 {
			continue
		}
		if err := s.syncIssuePlanning(ctx, record); err != nil {
			problems = append(problems, fmt.Errorf("sync issue planning status: %w", err))
		}
		if err := s.reconcileIssueRecord(ctx, record); err != nil {
			problems = append(problems, fmt.Errorf("issue attempt %s: %w", record.ID, err))
		}
		if _, err := s.IssueClaims.ScanPage("@reconcile", (index+1)%len(keys)+1); err != nil {
			return errors.Join(append(problems, err)...)
		}
	}
	return errors.Join(problems...)
}

func (s Server) reconcileIssueRecord(ctx context.Context, record localruntime.IssueClaimRecord) error {
	lock, err := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil {
		return err
	}
	if lock == nil {
		// A continuation owns the worker lock while waiting for disk admission.
		// Check its known PR, but leave cleanup to the lock owner and a later pass.
		if record.RequestID == "" {
			return nil
		}
		if record.PRMerged() {
			s.cancelMergedIssueWorker(record.RequestID)
			return nil
		}
		if record.State != "queued" {
			return nil
		}
		knownPR, err := s.resolveIssuePR(record)
		if err != nil || knownPR == "" {
			return err
		}
		allowed, err := s.RepositoryAllowed(ctx, record.Run.GitHubIssue.Repository)
		if err != nil || !allowed {
			return err
		}
		pr, err := s.linkedIssuePR(ctx, record.Run.GitHubIssue.Repository, knownPR)
		if err != nil || !pr.Merged {
			return err
		}
		if err := s.saveMergedIssuePR(record, pr, "", false); err != nil {
			return err
		}
		s.cancelMergedIssueWorker(record.RequestID)
		return nil
	}
	// Retain the lock through verification/unlock. Release before dispatch;
	// admission serialization and the instance lock prevent local races.
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	record, err = s.IssueClaims.Load(record.ID)
	if err != nil {
		return err
	}
	if record.PRAlreadyMerged && !record.PRMerged() && record.PRURL != "" {
		return s.recordMergedIssuePR(ctx, record, issuePullRequest{URL: record.PRURL, Title: record.PRTitle})
	}
	if record.PRMerged() {
		return s.finishMergedIssue(ctx, record)
	}
	workerRecorded := record.Phase == "submitted" || record.Outcome != "" || !record.StartedAt.IsZero()
	cleanupPending := record.UnlockPending ||
		(record.LabelAdded && workerRecorded && record.Outcome != localruntime.IssueChecksPending)
	checkLinkedPR := true
	if (record.Fulfilled() || record.State == "blocked") && s.issueTaskHistoryHidden(record, record.RequestID) {
		_, checkLinkedPR = s.Broker.Task(record.RequestID)
	}
	if !checkLinkedPR && !cleanupPending {
		return nil
	}
	allowed, err := s.RepositoryAllowed(ctx, record.Run.GitHubIssue.Repository)
	if err != nil {
		return err
	}
	if !allowed && !record.LabelAdded && !record.UnlockPending {
		return nil
	}
	// Check the linked PR before resuming checks, waiting for retry, or
	// applying recovery holds. A sibling PR on the branch cannot settle it.
	var linkedPR issuePullRequest
	var linkedPRErr error
	var knownPR string
	linkedPRDeferred := !checkLinkedPR
	if allowed && checkLinkedPR {
		knownPR, linkedPRErr = s.resolveIssuePR(record)
		if knownPR != "" {
			linkedPRDeferred = s.linkedIssuePRDeferred(record.ID, knownPR)
		}
	}
	if allowed && knownPR != "" && !linkedPRDeferred {
		linkedPR, linkedPRErr = s.linkedIssuePR(ctx, record.Run.GitHubIssue.Repository, knownPR)
		if linkedPRErr == nil {
			if linkedPR.Merged {
				return s.recordMergedIssuePR(ctx, record, linkedPR)
			}
			if record.PRURL == "" && strings.EqualFold(linkedPR.URL, knownPR) {
				record.PRURL = linkedPR.URL
			}
			if err := s.recordIssuePRBlocker(&record, linkedPR, linkedPR.recoveryBlocker(record.Run.GitHubIssue)); err != nil {
				return err
			}
		}
		if record.Fulfilled() || record.State == "blocked" || record.PRRecoveryBlocker != "" {
			s.deferLinkedIssuePRPoll(record.ID, knownPR)
		}
	}
	if linkedPRErr != nil && !cleanupPending {
		if knownPR != "" {
			if time.Now().Before(record.NextAttemptAt) {
				return nil
			}
			if record.Outcome == localruntime.IssueChecksPending {
				return s.deferIssueRecord(record, linkedPRErr)
			}
		}
		return linkedPRErr
	}
	if record.Fulfilled() && !cleanupPending {
		s.publishVerifiedIssuePR(record)
		return nil
	}
	requiresExplicitRecovery := record.Incident == workflowIssueRecoveryIncident ||
		(record.State == "blocked" && record.Incident == localruntime.IssueIncidentBranchRecovery)
	if !cleanupPending && (linkedPRDeferred || requiresExplicitRecovery) {
		return nil
	}

	staleOwner := isStaleIssueClaim(record)
	if err := s.IssueClaims.CheckOwner(record.InstanceID); err != nil {
		if !errors.Is(err, localruntime.ErrIssueOwnershipChanged) {
			return err
		}
		if staleOwner && record.NextAttemptAt.IsZero() {
			return nil
		}
		blockStaleIssueClaim(&record)
		return s.IssueClaims.Save(record)
	}
	if record.Outcome == "error" && record.Incident != localruntime.IssueIncidentBranchRecovery &&
		strings.HasSuffix(record.FailureDetail(), legacyIssueBranchRenameFailure) {
		record.RunError = record.FailureDetail()
		record.Incident = localruntime.IssueIncidentBranchRecovery
		record.NextAttemptAt = time.Time{}
		if err := s.IssueClaims.Save(record); err != nil {
			return err
		}
	}
	// Reclassify stored rollout incidents because older classifiers accepted
	// ambiguous provisioning reports. Preserve the evidence before recovery
	// checks can replace the last error.
	if record.Outcome == "error" && record.Incident != localruntime.IssueIncidentBranchRecovery &&
		(issueExplicitRetryError(record.Incident) == "" || record.Incident == localruntime.IssueIncidentExternalRollout) {
		failure := record.FailureDetail()
		reason := failurefollowup.NonRemediableFailureReason(errors.New(failure))
		previousIncident := record.Incident
		truncated := failurefollowup.HasTruncatedErrorDetails(failure)
		var matchedLogs bool
		if truncated && s.IssueExternalPrerequisiteFromLogs != nil {
			var logReason string
			logReason, matchedLogs = s.IssueExternalPrerequisiteFromLogs(record.RequestID, failure)
			if logReason != "" {
				reason = logReason
			}
		}
		// Legacy records used Error for both terminal reports and mutable
		// recovery status. Only the terminal report can disprove a blocker.
		lowerFailure := strings.ToLower(failure)
		// Older harnesses wrapped confirmed storage errors in this diagnosis.
		// Ordinary agent reports can quote disk errors from earlier commands.
		_, reported, agentReport := strings.Cut(lowerFailure, " reported failure: ")
		legacyDiskFull := workspace.DiskFullDiagnostic(failure) &&
			(!agentReport || strings.HasPrefix(reported, "workspace storage ran out of space"))
		terminalEvidence := failure != "" && failure != issueExplicitRetryError(previousIncident) &&
			(record.RunError != "" ||
				strings.HasPrefix(lowerFailure, "codex: codex reported failure: failure:") ||
				strings.HasPrefix(lowerFailure, "claude: claude reported failure: failure:"))
		switch {
		case legacyDiskFull:
			record.Incident = localruntime.IssueIncidentWorkspaceStorage
		case reason == failurefollowup.UnmergedDependencyReason:
			record.Incident = localruntime.IssueIncidentUnmergedDependency
		case reason == failurefollowup.OpenDependencyReason:
			record.Incident = localruntime.IssueIncidentOpenDependency
		case reason == failurefollowup.HumanActionRequiredReason:
			record.Incident = localruntime.IssueIncidentHumanAction
		case reason == failurefollowup.ExternalEvidencePrerequisitesReason:
			record.Incident = localruntime.IssueIncidentExternalEvidence
		case reason == failurefollowup.ExternalRolloutPrerequisitesReason:
			record.Incident = localruntime.IssueIncidentExternalRollout
		case previousIncident == localruntime.IssueIncidentExternalRollout && (matchedLogs || (!truncated && terminalEvidence)):
			record.Incident = "missing_pr"
			if record.Error == issueExplicitRetryError(previousIncident) {
				record.Error = ""
			}
		}
		if record.Incident != previousIncident ||
			(record.Incident == localruntime.IssueIncidentExternalRollout && record.RunError == "" && terminalEvidence) {
			record.RunError = failure
			if issueExplicitRetryError(record.Incident) != "" {
				record.NextAttemptAt = time.Time{}
			}
			if err := s.IssueClaims.Save(record); err != nil {
				return err
			}
		}
	}
	if stopped, err := localruntime.IssueProcessesStopped(record); err != nil || !stopped {
		if staleOwner {
			return err
		}
		record.State = "waiting_for_worker"
		record.Error = "worker descendants may still be running; issue remains locked"
		if err != nil {
			record.Error = err.Error()
		}
		return s.IssueClaims.Save(record)
	}
	record.ProcessGroups = nil
	record.ProcessUncertain = false
	now := time.Now()
	if record.UnlockPending && now.Before(record.NextUnlockAt) {
		return nil
	}
	if !allowed {
		// Clean up the owned label while preserving the recorded outcome.
		if !record.LabelAdded {
			return nil
		}
		issue := record.Run.GitHubIssue
		cleanupCtx := githubutil.WithCaller(ctx, "issue_cleanup")
		remote, err := s.latestIssueAttempt(cleanupCtx, issue.Repository, issue.Number)
		if err != nil {
			return s.deferIssueUnlock(record.ID, err)
		}
		if remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID {
			// Another owner controls the label. Drop only our cleanup intent.
			record.LabelAdded, record.UnlockPending = false, false
			return s.IssueClaims.Save(record)
		}
		if err := s.unlockIssue(cleanupCtx, &record); err != nil {
			return s.deferIssueUnlock(record.ID, err)
		}
		return nil
	}

	cooling := now.Before(record.NextAttemptAt)
	// Failed admission can leave a label to clean up while dispatch cools down.
	if cooling && (record.Outcome != "" || !record.UnlockPending) && !(linkedPRErr != nil && cleanupPending) {
		return nil
	}
	if record.Outcome == localruntime.IssueChecksPending && linkedPRErr == nil && !linkedPRDeferred {
		if staleOwner || record.Control != "" || record.State == "blocked" || record.PRRecoveryBlocker != "" {
			return nil
		}
		lock.Close()
		lock = nil
		return s.resumeIssueChecks(ctx, record)
	}
	issue := record.Run.GitHubIssue
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		if record.UnlockPending {
			return s.deferIssueUnlock(record.ID, err)
		}
		return s.deferIssueRecord(record, err)
	}
	if staleOwner && (remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID) {
		return nil
	}
	resumingNewIntent := record.Outcome == "" && !record.RemoteRecorded && remote.Claim.Attempt < issue.Claim.Attempt
	if remote.Claim.ID != "" && !resumingNewIntent && (remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID) {
		if isUnadmittedWorkflowIssue(record) {
			blockObservedWorkflowIssue(&record)
			return s.IssueClaims.Save(record)
		}
		record.State = "blocked"
		record.Error = "ownership changed; explicit recovery required"
		return s.IssueClaims.Save(record)
	}
	if record.RemoteRecorded && remote.Claim.ID == "" {
		record.State = "blocked"
		record.Error = "remote ownership record is missing; explicit recovery required"
		return s.IssueClaims.Save(record)
	}
	if workerRecorded {
		if record.StoppedAt.IsZero() {
			record.StoppedAt = time.Now().UTC()
			record.Outcome = "interrupted"
			record.Incident = "interrupted"
			if !staleOwner {
				record.Error = "worker exited before recording its outcome"
			}
		}
		record.UnlockPending = record.LabelAdded
	}
	if (record.Outcome == "interrupted" || record.Outcome == "stopped") && record.ExecutionAgentAttempts != nil {
		// Persist the outcome before removing the reserve. A missing
		// reserve on an unfinished execution signals journal exhaustion.
		if err := s.IssueClaims.Save(record); err != nil {
			return fmt.Errorf("persist issue outcome before releasing execution reserve: %w", err)
		}
		if err := s.IssueClaims.ReleaseExecutionReserve(record.ID); err != nil {
			return fmt.Errorf("release issue execution reserve: %w", err)
		}
		record.ExecutionAgentAttempts = nil
	}
	// Unlock must remain possible even if PR lookup or settings are unavailable.
	if record.UnlockPending && remote.Claim.ID == record.ID {
		if err := s.unlockIssue(ctx, &record); err != nil {
			return s.deferIssueUnlock(record.ID, err)
		}
	}
	if linkedPRErr != nil {
		return linkedPRErr
	}
	if linkedPRDeferred || cooling || record.Fulfilled() || record.PRRecoveryBlocker != "" || requiresExplicitRecovery {
		return nil
	}
	if app.IsWorkflowPRMetadataFailure(record.FailureDetail()) ||
		(record.PRVerification == "" && record.Outcome != "" && record.Outcome != "completed" && record.Outcome != workflow.OutcomeReviewPending &&
			record.Run.Workflow != nil && record.Run.Workflow.ResumePRURL != "") {
		record.State = "blocked"
		record.Error = record.FailureDetail()
		record.NextAttemptAt = time.Time{}
		return s.IssueClaims.Save(record)
	}
	// Branch conflicts need explicit recovery with evidence from the saved
	// workspace before a matching PR can fulfill the claim.
	ownsBranch := remote.Claim.ID == record.ID && record.BranchReserved
	if (ownsBranch || record.Outcome == "") && !requiresIssueBranchRecovery(record) {
		pr := linkedPR
		if knownPR == "" {
			pr, err = s.findIssuePR(ctx, issue)
			if err != nil {
				return s.deferIssueRecord(record, err)
			}
		}
		if err := pr.recoveryBlocker(issue); err != nil {
			if record.Outcome == "" {
				return s.deferIssueRecord(record, err)
			}
			if err := s.recordIssuePRBlocker(&record, pr, err); err != nil {
				return err
			}
			s.deferLinkedIssuePRPoll(record.ID, pr.URL)
			return nil
		}
		if ownsBranch && pr.URL != "" && !pr.Closed && pr.BaseBranch == issue.Claim.BaseBranch && !pr.resumesVerification(record) {
			if pr.Merged {
				return s.recordMergedIssuePR(ctx, record, pr)
			}
			if record.Outcome == "interrupted" {
				if err := s.verifyRecoveredIssuePR(ctx, record, pr); err != nil {
					return s.deferIssueRecord(record, err)
				}
			}
			return s.recordVerifiedIssuePR(&record, pr)
		}
	}
	// A restored owner can finalize a stopped attempt. Dispatch still requires
	// an explicit retry.
	if staleOwner {
		return s.IssueClaims.Save(record)
	}
	if record.Incident == localruntime.IssueIncidentBranchRecovery {
		action := record.FailureDetail()
		if strings.HasSuffix(action, legacyIssueBranchRenameFailure) {
			action = fmt.Sprintf("%s: reconcile %q and its existing pull request, then use Recover / retry",
				app.ErrIssueBranchRecovery, issue.Claim.Branch)
		}
		blockIssueBranchRecovery(&record, action)
		return s.IssueClaims.Save(record)
	}
	if record.Outcome != "" && record.Incident == "" {
		record.Incident = "missing_pr"
	}
	policy := s.IssueRecovery.Defaults()
	explicitRetryError := issueExplicitRetryError(record.Incident)
	if record.Control != "" || record.Outcome == "stopped" || record.Outcome == "no_changes" ||
		explicitRetryError != "" || record.AgentAttempts >= policy.MaxAttempts || issueRunFailures(record) >= policy.MaxAttempts {
		record.State = "blocked"
		switch {
		case record.Outcome == "stopped":
			record.Error = "stopped by operator; explicit resume required"
		case record.Outcome == "no_changes":
			record.Error = "no PR produced; review no-change evidence before retrying"
		case explicitRetryError != "":
			record.Error = explicitRetryError
			record.NextAttemptAt = time.Time{}
		case issueRunFailures(record) >= policy.MaxAttempts:
			record.Error = "workflow retry budget exhausted; explicit retry or deployed fix required"
		default:
			record.Error = "agent retry budget exhausted; explicit retry or deployed fix required"
		}
		if record.Control != "" {
			record.Error = "operator " + record.Control + "; explicit resume required"
		}
		return s.IssueClaims.Save(record)
	}
	settings, enabled, err := s.workflowIssueSettings(record.Run)
	if err != nil {
		return s.deferIssueRecord(record, err)
	}
	if !enabled {
		record.State = "blocked"
		record.Error = "automatic issue retry requires an enabled workflow"
		record.NextAttemptAt = time.Time{}
		return s.IssueClaims.Save(record)
	}
	if record.Outcome != "" && record.NextAttemptAt.IsZero() {
		record.State = "retry_wait"
		record.NextAttemptAt = time.Now().Add(policy.RetryDelay(record.ID, issueRetryFailures(record)))
		return s.IssueClaims.Save(record)
	}
	if s.IssueReady != nil {
		if err := s.IssueReady(ctx); err != nil {
			return s.deferIssueRecord(record, err)
		}
	}
	lock.Close()
	lock = nil
	if record.Outcome == "" {
		if record.BranchReserved {
			exists, err := s.issueBranchExists(ctx, issue.Repository, issue.Claim.Branch)
			if err != nil {
				return s.deferIssueRecord(record, err)
			}
			if !exists {
				return s.deferIssueRecord(record, missingReservedIssueRef(issue.Claim.Branch))
			}
		}
		var fresh GitHubIssue
		fresh, err = s.fetchGitHubIssue(ctx, issue.Repository, issue.Number)
		if err != nil {
			return s.deferIssueRecord(record, err)
		}
		var login string
		login, err = s.issueGitHubLogin(ctx)
		if err != nil {
			return s.deferIssueRecord(record, err)
		}
		// Our durable claim may already have applied its own exclusion label.
		// Only ignore that label, never a newly configured exclusion label.
		if record.LabelAdded {
			labels := make([]GitHubIssueLabel, 0, len(fresh.Labels))
			for _, label := range fresh.Labels {
				if !strings.EqualFold(label.Name, issue.Claim.ExclusionLabel) {
					labels = append(labels, label)
				}
			}
			fresh.Labels = labels
		}
		if reason := issueEligibilityReason(fresh, settings, login); reason != "" {
			return s.deferIssueRecord(record, fmt.Errorf("issue is ineligible: %s", reason))
		}
		if record.IssueNodeID == "" {
			record, err = s.prepareObservedIssue(ctx, record, settings, login)
			if err != nil {
				return s.deferIssueRecord(record, err)
			}
		}
		if record.ReservationSHA == "" {
			ref := issue.Claim.BaseBranch
			if record.BranchReserved {
				ref = issue.Claim.Branch
			}
			record.ReservationSHA, err = s.issueBranchSHA(ctx, issue.Repository, ref)
			if err != nil {
				return s.deferIssueRecord(record, err)
			}
			if err := s.IssueClaims.Save(record); err != nil {
				return err
			}
		}
		_, err = s.resumeIssueAdmission(ctx, record, s.issueRetrySubmitter())
	} else {
		_, _, err = s.recoverIssueClaimWithRun(ctx, issue.Repository, issue.Number, remote, s.issueRetrySubmitter(), true, nil)
	}
	if err != nil {
		var deferred *issueAdmissionDeferredError
		if errors.As(err, &deferred) {
			return err
		}
		return s.deferIssueRecord(record, err)
	}
	return nil
}

func (s Server) resumeIssueChecks(ctx context.Context, record localruntime.IssueClaimRecord) error {
	if record.Run.Workflow != nil {
		_, enabled, err := s.workflowIssueSettings(record.Run)
		if err != nil {
			return s.deferIssueRecord(record, err)
		}
		if !enabled {
			return s.deferIssueRecord(record, fmt.Errorf("check verification paused until the originating workflow is enabled with its original policy"))
		}
	}
	if err := s.ValidateIssueOwnership(ctx, record.Run); err != nil {
		return s.deferIssueRecord(record, err)
	}
	submit := s.issueRetrySubmitter()
	if submit == nil {
		return s.deferIssueRecord(record, fmt.Errorf("issue submission is unavailable"))
	}
	body, err := json.Marshal(record.Run)
	if err == nil {
		_, err = submit(ctx, body)
	}
	if err != nil {
		return s.deferIssueRecord(record, err)
	}
	return nil
}

func (s Server) recordVerifiedIssuePR(record *localruntime.IssueClaimRecord, pr issuePullRequest) error {
	record.PRURL = pr.URL
	record.PRTitle = strings.TrimSpace(pr.Title)
	record.VerifiedAt = time.Now().UTC()
	record.State = "fulfilled"
	record.Error = ""
	record.NextAttemptAt = time.Time{}
	if err := s.IssueClaims.Save(*record); err != nil {
		return err
	}
	s.publishVerifiedIssuePR(*record)
	return nil
}

func (s Server) recordMergedIssuePR(ctx context.Context, record localruntime.IssueClaimRecord, pr issuePullRequest) error {
	if err := s.saveMergedIssuePR(record, pr, "", false); err != nil {
		return err
	}
	record, err := s.IssueClaims.Load(record.ID)
	if err != nil {
		return err
	}
	return s.finishMergedIssue(ctx, record)
}

func (s Server) knownIssuePR(record localruntime.IssueClaimRecord) string {
	if record.PRURL != "" {
		return record.PRURL
	}
	if task, ok := s.Broker.Task(record.RequestID); ok && task.PRURL != "" {
		return task.PRURL
	}
	if record.Run.Workflow != nil {
		return record.Run.Workflow.ResumePRURL
	}
	return ""
}

func (s Server) linkedIssuePRDeferred(claimID, prURL string) bool {
	s.issueAutomation.mu.Lock()
	defer s.issueAutomation.mu.Unlock()
	return s.Broker.now().Before(s.issueAutomation.linkedPRPollAfter[claimID+" "+strings.ToLower(prURL)])
}

func (s Server) deferLinkedIssuePRPoll(claimID, prURL string) {
	s.issueAutomation.mu.Lock()
	defer s.issueAutomation.mu.Unlock()
	if s.issueAutomation.linkedPRPollAfter == nil {
		s.issueAutomation.linkedPRPollAfter = make(map[string]time.Time)
	}
	s.issueAutomation.linkedPRPollAfter[claimID+" "+strings.ToLower(prURL)] = s.Broker.now().Add(retainedIssuePRPollInterval)
}

func (s Server) recordIssuePRBlocker(record *localruntime.IssueClaimRecord, pr issuePullRequest, cause error) error {
	blocker := ""
	if cause != nil {
		blocker = cause.Error()
	}
	if record.PRRecoveryBlocker == blocker {
		return nil
	}
	record.PRRecoveryBlocker = blocker
	return s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if !current.PRMerged() {
			current.PRRecoveryBlocker = blocker
			if blocker != "" && current.PRURL == "" {
				current.PRURL = pr.URL
			}
		}
		return nil
	})
}

// Resolve the workflow journal outside claim updates. Workflow merge callbacks
// write the claim while holding the journal lock.
func (s Server) resolveIssuePR(record localruntime.IssueClaimRecord) (string, error) {
	known := s.knownIssuePR(record)
	policy := record.Run.Workflow
	if known != "" || policy == nil || policy.EventID == "" || s.Workflows == nil || record.PRMerged() {
		return known, nil
	}
	runs, err := s.Workflows.Runs()
	if err != nil {
		return "", fmt.Errorf("read workflow journal for linked issue pull request: %w", err)
	}
	for _, run := range runs {
		if run.ID == policy.EventID && run.Item.Kind == "issue" &&
			(run.RequestID == "" || run.RequestID == record.RequestID) {
			return run.LinkedPRURL(), nil
		}
	}
	return "", nil
}

func (s Server) saveMergedIssuePR(record localruntime.IssueClaimRecord, pr issuePullRequest, reviewDetail string, mergedByApp bool) error {
	return s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if known := s.knownIssuePR(*current); known != "" && !strings.EqualFold(known, pr.URL) {
			return fmt.Errorf("merged pull request does not match the issue task's linked pull request")
		}
		current.PRURL = pr.URL
		if title := strings.TrimSpace(pr.Title); title != "" {
			current.PRTitle = title
		}
		if !pr.MergedAt.IsZero() {
			current.PRMergedAt = pr.MergedAt
		}
		current.PRMergedByApp = current.PRMergedByApp || mergedByApp
		current.PRAlreadyMerged = true
		current.Outcome = "completed"
		current.Incident = ""
		if reviewDetail != "" {
			current.ReviewDetail = reviewDetail
		}
		current.PRRecoveryBlocker = ""
		if !current.PRMerged() {
			current.UnlockPending = current.LabelAdded
			current.State = localruntime.IssueStateMerged
			if current.PRVerification != "" {
				current.RunError = current.FailureDetail()
				current.VerifiedAt = time.Time{}
				current.State = localruntime.IssueStateMergedUnverified
				current.Error = mergedUnverifiedIssueError
			} else if current.VerifiedAt.IsZero() {
				current.VerifiedAt = time.Now().UTC()
			}
		}
		current.NextAttemptAt = time.Time{}
		return nil
	})
}

func (s Server) finishMergedIssue(ctx context.Context, record localruntime.IssueClaimRecord) (err error) {
	if s.Workflows != nil && record.Run.Workflow != nil && !record.Run.Workflow.IsDefaultExecution() {
		defer func() {
			err = errors.Join(err, s.Workflows.ObserveIssueMerge(record.Run.Workflow.EventID, record.RequestID, record.PRURL))
		}()
	}
	s.publishVerifiedIssuePR(record)
	task, ok := s.Broker.Task(record.RequestID)
	markMerged := ok && task.Status != "merged" && (task.PRURL == "" || strings.EqualFold(task.PRURL, record.PRURL))
	if markMerged && !isCompletedTaskStatus(task.Status) {
		s.cancelMergedIssueWorker(record.RequestID)
	}
	if stopped, err := localruntime.IssueProcessesStopped(record); err != nil || !stopped {
		return err
	}
	if markMerged {
		mergedAt := formatTimeOrEmpty(record.PRMergedAt)
		task.PRURL = record.PRURL
		s.Broker.RecordReleaseFromTask(task, mergedAt)
		if err := s.Broker.MarkTaskPRMerged(record.RequestID, mergedAt); err == nil && record.Fulfilled() {
			cleanupMergedTask(ctx, s.Broker, s.CloseTask, s.logf, task)
		}
	}
	defer func() {
		err = errors.Join(err, s.backfillIssueReleaseMergeTime(ctx, record))
	}()
	if record.ExecutionAgentAttempts != nil {
		if err := s.IssueClaims.ReleaseExecutionReserve(record.ID); err != nil {
			return err
		}
		record.ExecutionAgentAttempts = nil
		if err := s.IssueClaims.Save(record); err != nil {
			return err
		}
	}
	if (!record.LabelAdded && !record.UnlockPending) || time.Now().Before(record.NextUnlockAt) {
		return nil
	}
	if err := s.IssueClaims.CheckOwner(record.InstanceID); err != nil {
		if errors.Is(err, localruntime.ErrIssueOwnershipChanged) {
			return nil
		}
		return err
	}
	issue := record.Run.GitHubIssue
	ctx = githubutil.WithCaller(ctx, "issue_cleanup")
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		return s.deferIssueUnlock(record.ID, err)
	}
	if remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID {
		record.LabelAdded, record.UnlockPending = false, false
		return s.IssueClaims.Save(record)
	}
	if err := s.unlockIssue(ctx, &record); err != nil {
		return s.deferIssueUnlock(record.ID, err)
	}
	return nil
}

func (s Server) backfillIssueReleaseMergeTime(ctx context.Context, record localruntime.IssueClaimRecord) error {
	// Terminal cleanup can finish before GitHub's timestamp becomes available.
	// Retry optional metadata within the task history retention window.
	if record.PRMergedAt.IsZero() && record.PRURL != "" &&
		s.Broker.now().Sub(record.UpdatedAt) < defaultClosedTaskRetention &&
		!s.linkedIssuePRDeferred(record.ID, record.PRURL) {
		s.deferLinkedIssuePRPoll(record.ID, record.PRURL)
		repo := record.Run.GitHubIssue.Repository
		if allowed, err := s.RepositoryAllowed(ctx, repo); err == nil && allowed {
			if pr, err := s.linkedIssuePR(ctx, repo, record.PRURL); err == nil && pr.Merged && !pr.MergedAt.IsZero() {
				if err := s.saveMergedIssuePR(record, pr, "", false); err != nil {
					return err
				}
				record.PRMergedAt = pr.MergedAt
			}
		}
	}
	s.Broker.backfillReleaseMergeTime(record.RequestID, record.PRURL, record.PRMergedAt)
	return nil
}

func (s Server) cancelMergedIssueWorker(requestID string) {
	if s.CancelMergedTask != nil {
		s.CancelMergedTask(requestID)
	}
}

func (s Server) publishVerifiedIssuePR(record localruntime.IssueClaimRecord) {
	if s.Broker != nil {
		s.Broker.recordVerifiedTaskPR(record.RequestID, verifiedTaskPR{
			URL: record.PRURL, Title: record.PRTitle, VerifiedAt: record.VerifiedAt,
		})
	}
}

// ValidateIssueOwnership fences harness-controlled execution and publication.
// Agent subprocesses also retain the local lock; recovery never steals their
// claim merely because the daemon or a heartbeat disappeared.
func (s Server) ValidateIssueOwnership(ctx context.Context, run config.Config) error {
	if run.GitHubIssue == nil || run.GitHubIssue.Claim == nil {
		return nil
	}
	issue := run.GitHubIssue
	if s.IssueClaims != nil {
		record, err := s.IssueClaims.Load(issue.Claim.ID)
		if err != nil {
			return err
		}
		if record.PRMerged() {
			return errors.New("issue task pull request is already merged")
		}
	}
	remote, err := s.latestIssueAttempt(githubutil.WithCaller(ctx, "issue_ownership"), issue.Repository, issue.Number)
	if err != nil {
		return err
	}
	if remote.Claim.ID != issue.Claim.ID || remote.InstanceID != s.instanceID() {
		return localruntime.ErrIssueOwnershipChanged
	}
	return nil
}

// Count a terminal outcome once per claim. Each automatic successor carries
// the previous count, including runs that never reached the agent step.
func issueRunFailures(record localruntime.IssueClaimRecord) int {
	if record.Fulfilled() || record.PRMerged() || record.PRAlreadyMerged {
		return 0
	}
	failures := record.PriorRunFailures
	if record.Incident == localruntime.IssueIncidentWorkspaceStorage {
		return failures
	}
	switch record.Outcome {
	case "error", "completed", "no_changes", workflow.OutcomeReviewPending:
		failures++
	case "interrupted":
		if record.Incident != "service_shutdown" {
			failures++
		}
	}
	return failures
}

func issueRetryFailures(record localruntime.IssueClaimRecord) int {
	return max(record.AgentAttempts, record.Failures+issueRunFailures(record))
}

func (s Server) scheduleIssueRetry(record *localruntime.IssueClaimRecord, cause error) {
	var rate *githubutil.RateLimitError
	if errors.As(cause, &rate) {
		record.NextAttemptAt = rate.RetryAt
		return
	}
	record.Failures++
	record.NextAttemptAt = s.IssueRecovery.Defaults().RetryDeadline(time.Now(), record.ID, issueRetryFailures(*record), cause)
}

func (s Server) deferIssueRecord(record localruntime.IssueClaimRecord, cause error) error {
	// Merge, because submission may have launched a worker before returning an
	// ambiguous receipt. Never overwrite a running or completed worker here.
	err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if current.PRMerged() {
			return nil
		}
		if current.StartedAt.After(record.StartedAt) || current.StoppedAt.After(record.StoppedAt) {
			return nil
		}
		if errors.Is(cause, localruntime.ErrIssueOwnershipChanged) {
			blockStaleIssueClaim(current)
			return nil
		}
		var blocker issuePRRecoveryBlocker
		if errors.As(cause, &blocker) && s.knownIssuePR(*current) != "" {
			current.PRRecoveryBlocker = blocker.Error()
			return nil
		}
		if current.Outcome == localruntime.IssueChecksPending {
			if current.UpdatedAt.After(record.UpdatedAt) {
				return nil
			}
			current.State = localruntime.IssueChecksPending
			current.Error = cause.Error()
			var rate *githubutil.RateLimitError
			if errors.As(cause, &rate) {
				current.NextAttemptAt = rate.RetryAt
			} else {
				current.Failures++
				current.NextAttemptAt = s.IssueRecovery.Defaults().RetryDeadline(time.Now(), current.ID, current.Failures, cause)
			}
			return nil
		}
		if errors.Is(cause, errWorkflowIssueRecovery) {
			blockObservedWorkflowIssue(current)
			return nil
		}
		if current.Incident == localruntime.IssueIncidentWorkspaceStorage {
			// Remote verification can back off while the storage hold stays visible.
			current.State = "blocked"
			current.Error = issueExplicitRetryError(current.Incident)
			current.NextAttemptAt = s.IssueRecovery.Defaults().RetryDeadline(time.Now(), current.ID, issueRetryFailures(*current), cause)
			return nil
		}
		if errors.Is(cause, app.ErrIssueBranchRecovery) {
			blockIssueBranchRecovery(current, cause.Error())
			return nil
		}
		if !isStaleIssueClaim(*current) {
			current.State = "retry_wait"
			current.Error = cause.Error()
		}
		s.scheduleIssueRetry(current, cause)
		return nil
	})
	return errors.Join(cause, err)
}

const legacyIssueBranchRenameFailure = "reserved issue branch cannot be renamed; explicit recovery is required"

func requiresIssueBranchRecovery(record localruntime.IssueClaimRecord) bool {
	return record.Incident == localruntime.IssueIncidentBranchRecovery ||
		strings.HasSuffix(record.FailureDetail(), legacyIssueBranchRenameFailure)
}

func missingReservedIssueRef(branch string) error {
	return fmt.Errorf("%w: restore the remote ref %q to the intended issue commits, then use Recover / retry",
		app.ErrIssueBranchRecovery, "refs/heads/"+branch)
}

func blockIssueBranchRecovery(record *localruntime.IssueClaimRecord, action string) {
	record.State = "blocked"
	record.Incident = localruntime.IssueIncidentBranchRecovery
	record.Error = action
	record.NextAttemptAt = time.Time{}
	record.UnlockPending = record.LabelAdded
}

func isStaleIssueClaim(record localruntime.IssueClaimRecord) bool {
	return record.State == "blocked" && record.Error == localruntime.ErrIssueOwnershipChanged.Error()
}

func blockStaleIssueClaim(record *localruntime.IssueClaimRecord) {
	record.State = "blocked"
	record.Error = localruntime.ErrIssueOwnershipChanged.Error()
	record.NextAttemptAt = time.Time{}
}

func (s Server) deferIssueUnlock(id string, cause error) error {
	err := s.IssueClaims.Update(id, func(record *localruntime.IssueClaimRecord) error {
		record.UnlockPending = true
		record.UnlockError = cause.Error()
		var rate *githubutil.RateLimitError
		if errors.As(cause, &rate) {
			record.NextUnlockAt = rate.RetryAt
		} else {
			record.UnlockFailures++
			record.NextUnlockAt = s.IssueRecovery.Defaults().RetryDeadline(time.Now(), record.ID, record.UnlockFailures, cause)
		}
		return nil
	})
	return errors.Join(cause, err)
}

func (s Server) unlockIssue(ctx context.Context, record *localruntime.IssueClaimRecord) error {
	issue := record.Run.GitHubIssue
	// Read first: deletion after an accepted-but-lost response is idempotent.
	fresh, err := s.fetchGitHubIssueMetadata(ctx, issue.Repository, issue.Number)
	if err != nil {
		return err
	}
	if githubIssueHasAnyLabel(fresh, []string{issue.Claim.ExclusionLabel}) {
		_, err = s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "--method", "DELETE", fmt.Sprintf("repos/%s/issues/%d/labels/%s", issue.Repository, issue.Number, url.PathEscape(issue.Claim.ExclusionLabel))}})
		if err != nil {
			return err
		}
	}
	record.LabelAdded = false
	record.UnlockPending = false
	record.NextUnlockAt = time.Time{}
	record.UnlockFailures = 0
	record.UnlockError = ""
	return s.IssueClaims.Save(*record)
}

type issuePullRequest struct {
	URL        string
	Title      string
	HeadSHA    string
	BaseBranch string
	Closed     bool
	Merged     bool
	MergedAt   time.Time
}

type githubIssuePullRequest struct {
	Number   int
	Title    string
	State    string
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref  string
		SHA  string
		Repo struct {
			FullName string `json:"full_name"`
		}
	}
	Base struct {
		Ref  string
		Repo struct {
			FullName string `json:"full_name"`
		}
	}
}

func (pr githubIssuePullRequest) issuePR(repo string) issuePullRequest {
	if pr.Number <= 0 {
		return issuePullRequest{}
	}
	result := issuePullRequest{
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, pr.Number), Title: pr.Title,
		HeadSHA: pr.Head.SHA, BaseBranch: pr.Base.Ref,
		Closed: pr.State == "closed" && pr.MergedAt == nil, Merged: pr.MergedAt != nil,
	}
	if pr.MergedAt != nil {
		result.MergedAt = *pr.MergedAt
	}
	return result
}

func (s Server) linkedIssuePR(ctx context.Context, repo, prURL string) (issuePullRequest, error) {
	if !strings.EqualFold(githubutil.PullRequestRepository(prURL), repo) {
		return issuePullRequest{}, fmt.Errorf("linked pull request must belong to the issue repository")
	}
	path := "repos/" + repo + "/pulls/" + githubutil.PullRequestSelector(prURL)
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", path}})
	if err != nil {
		return issuePullRequest{}, err
	}
	var pr githubIssuePullRequest
	if err := json.Unmarshal([]byte(result.Stdout), &pr); err != nil {
		return issuePullRequest{}, fmt.Errorf("decode linked pull request: %w", err)
	}
	return pr.issuePR(repo), nil
}

func (pr issuePullRequest) resumesVerification(record localruntime.IssueClaimRecord) bool {
	return pr.URL != "" && !pr.Closed && !pr.Merged && (record.PRVerification != "" ||
		(record.Outcome == workflow.OutcomeReviewPending && record.PRURL == pr.URL)) &&
		pr.BaseBranch == record.Run.GitHubIssue.Claim.BaseBranch
}

type issuePRRecoveryBlocker string

func (e issuePRRecoveryBlocker) Error() string {
	return fmt.Sprintf("%s: %s before using Recover / retry", app.ErrIssueBranchRecovery, string(e))
}

func (e issuePRRecoveryBlocker) Unwrap() error { return app.ErrIssueBranchRecovery }

func (pr issuePullRequest) recoveryBlocker(issue *config.GitHubIssueConfig) error {
	if pr.URL == "" {
		return nil
	}
	action := ""
	switch {
	case pr.Closed && pr.BaseBranch != issue.Claim.BaseBranch:
		action = fmt.Sprintf("reopen the existing pull request for %q and retarget it to %q", issue.Claim.Branch, issue.Claim.BaseBranch)
	case pr.Closed:
		action = fmt.Sprintf("reopen the existing pull request for %q", issue.Claim.Branch)
	case pr.BaseBranch != issue.Claim.BaseBranch:
		action = fmt.Sprintf("retarget the existing pull request for %q to %q", issue.Claim.Branch, issue.Claim.BaseBranch)
	}
	if action != "" {
		return issuePRRecoveryBlocker(action)
	}
	return nil
}

// Claimed issue runs have exactly one checkout, under repo/ in the workspace.
func savedIssueCheckout(record localruntime.IssueClaimRecord) (string, error) {
	if record.WorkspaceDir == "" {
		return "", nil
	}
	dir := filepath.Join(record.WorkspaceDir, "repo")
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("saved checkout Git metadata is not a directory")
	}
	return dir, nil
}

func verifyIssueRetryCheckout(ctx context.Context, record localruntime.IssueClaimRecord, branch string) error {
	action := fmt.Errorf("%w: preserve work at the current HEAD and check out %q in the saved workspace, then use Recover / retry",
		app.ErrIssueBranchRecovery, branch)
	dir, err := savedIssueCheckout(record)
	if err != nil {
		return fmt.Errorf("%w: inspect saved checkout: %v", action, err)
	}
	if dir == "" {
		return nil
	}
	gitCtx, cancel := context.WithTimeout(ctx, githubIssueCommandTimeout)
	defer cancel()
	result, err := (execx.OSRunner{}).Run(gitCtx, execx.Command{Dir: dir, Name: "git",
		Args: []string{"--git-dir=.git", "symbolic-ref", "--quiet", "HEAD"}})
	if err != nil || strings.TrimSpace(result.Stdout) != "refs/heads/"+branch {
		return action
	}
	return nil
}

func (s Server) verifyRecoveredIssuePR(ctx context.Context, record localruntime.IssueClaimRecord, pr issuePullRequest) error {
	issue := record.Run.GitHubIssue
	action := fmt.Errorf("%w: verify the recovered commits in the saved workspace, publish them on %q in the existing pull request, then use Recover / retry",
		app.ErrIssueBranchRecovery, issue.Claim.Branch)
	if pr.HeadSHA == "" {
		return action
	}
	dir, err := savedIssueCheckout(record)
	if err != nil {
		return fmt.Errorf("%w: inspect saved checkout: %v", action, err)
	}
	if dir != "" {
		// Inspect the workspace without updating the index or walking parent repos.
		gitCtx, cancel := context.WithTimeout(ctx, githubIssueCommandTimeout)
		defer cancel()
		runner := execx.OSRunner{}
		status, err := runner.Run(gitCtx, execx.Command{Dir: dir, Name: "git",
			Args: []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "--git-dir=.git", "--work-tree=.", "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"}})
		if err != nil || status.Stdout != "" {
			return action
		}
		head, err := runner.Run(gitCtx, execx.Command{Dir: dir, Name: "git",
			Args: []string{"--git-dir=.git", "rev-parse", "--verify", "HEAD"}})
		if err != nil || strings.TrimSpace(head.Stdout) != pr.HeadSHA {
			return action
		}
	} else if record.AgentAttempts != 0 || record.PublicationReady ||
		strings.HasSuffix(record.FailureDetail(), legacyIssueBranchRenameFailure) ||
		(record.WorkspaceDir != "" && record.ExitCode != app.ExitClone) {
		return action
	}
	if !pr.Merged {
		sha, err := s.issueBranchSHA(ctx, issue.Repository, issue.Claim.Branch)
		if err != nil {
			return fmt.Errorf("verify the reserved branch for the existing pull request: %w", err)
		}
		if sha != pr.HeadSHA {
			if dir == "" {
				return fmt.Errorf("%w: reconcile the remote ref %q with the existing pull request's head, then use Recover / retry",
					app.ErrIssueBranchRecovery, "refs/heads/"+issue.Claim.Branch)
			}
			return action
		}
	}
	return nil
}

// Prefer a PR that can fulfill the claim. Keep unmerged PRs from any base
// across pages so recovery can require reopening or retargeting before dispatch.
func (s Server) findIssuePR(ctx context.Context, issue *config.GitHubIssueConfig) (issuePullRequest, error) {
	owner, _, _ := strings.Cut(issue.Repository, "/")
	var blocker issuePullRequest
	for page := 1; ; page++ {
		query := url.Values{"state": {"all"}, "head": {owner + ":" + issue.Claim.Branch}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + issue.Repository + "/pulls?" + query.Encode()}})
		if err != nil {
			return issuePullRequest{}, err
		}
		var prs []githubIssuePullRequest
		if err := json.Unmarshal([]byte(result.Stdout), &prs); err != nil {
			return issuePullRequest{}, err
		}
		for _, pr := range prs {
			if pr.Number <= 0 || pr.Head.Ref != issue.Claim.Branch ||
				!strings.EqualFold(pr.Head.Repo.FullName, issue.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, issue.Repository) {
				continue
			}
			match := pr.issuePR(issue.Repository)
			if match.BaseBranch == issue.Claim.BaseBranch && (pr.State == "open" || match.Merged) {
				return match, nil
			}
			if (pr.State == "open" || match.Closed) && blocker.URL == "" {
				blocker = match
			}
		}
		if len(prs) < 100 {
			return blocker, nil
		}
	}
}
