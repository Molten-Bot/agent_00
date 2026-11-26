package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const mergedUnverifiedIssueError = "pull request merged before draft verification passed"

func (s Server) runIssueRecovery(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		err := s.ReconcileIssueWork(ctx)
		var status issueAutomationStatus
		if err != nil && !errors.Is(err, errIssueRecoveryDeferred) && ctx.Err() == nil {
			status.addError(err)
			s.logf("localruntime.ui status=warn event=github_issue_recovery err=%q", status.Error)
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	if record.Incident == workflowIssueRecoveryIncident {
		return nil
	}
	if !record.UnlockPending {
		if record.Fulfilled() {
			s.publishVerifiedIssuePR(record)
			return nil
		}
		if record.State == localruntime.IssueStateMergedUnverified {
			return nil
		}
	}
	if record.State == "blocked" && record.Incident == localruntime.IssueIncidentBranchRecovery && !record.UnlockPending {
		return nil
	}
	lock, err := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
	if err != nil {
		return err
	}
	if lock == nil {
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
	cooling := now.Before(record.NextAttemptAt)
	// Failed admission can leave a label to clean up while dispatch cools down.
	if cooling && (record.Outcome != "" || !record.UnlockPending) {
		return nil
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
	if record.Phase == "submitted" || record.Outcome != "" || !record.StartedAt.IsZero() {
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
	if record.Outcome == "interrupted" && record.ExecutionAgentAttempts != nil {
		// Persist the interruption before removing the reserve. A missing
		// reserve on an unfinished execution signals journal exhaustion.
		if err := s.IssueClaims.Save(record); err != nil {
			return fmt.Errorf("persist interrupted issue outcome: %w", err)
		}
		if err := s.IssueClaims.ReleaseExecutionReserve(record.ID); err != nil {
			return fmt.Errorf("release interrupted issue execution reserve: %w", err)
		}
		record.ExecutionAgentAttempts = nil
	}
	// Unlock must remain possible even if PR lookup or settings are unavailable.
	if record.UnlockPending && remote.Claim.ID == record.ID {
		if err := s.unlockIssue(ctx, &record); err != nil {
			return s.deferIssueUnlock(record.ID, err)
		}
	}
	if cooling || record.Fulfilled() || record.State == localruntime.IssueStateMergedUnverified {
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
		pr, err := s.findIssuePR(ctx, issue)
		if err != nil {
			return s.deferIssueRecord(record, err)
		}
		if record.Outcome == "" {
			if err := pr.recoveryBlocker(issue); err != nil {
				return s.deferIssueRecord(record, err)
			}
		}
		if ownsBranch && pr.URL != "" && !pr.Closed && pr.BaseBranch == issue.Claim.BaseBranch && !pr.resumesVerification(record) {
			if pr.mergedUnverified(record) {
				return s.recordMergedUnverifiedIssuePR(record, pr)
			}
			if record.Outcome == "interrupted" {
				if err := s.verifyRecoveredIssuePR(ctx, record, pr); err != nil {
					return s.deferIssueRecord(record, err)
				}
			}
			return s.recordVerifiedIssuePR(record, pr)
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
		record.NextAttemptAt = time.Now().Add(issueRetryDelay(record.ID, issueRetryFailures(record), policy))
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

func (s Server) recordVerifiedIssuePR(record localruntime.IssueClaimRecord, pr issuePullRequest) error {
	record.PRURL = pr.URL
	record.PRTitle = strings.TrimSpace(pr.Title)
	record.VerifiedAt = time.Now().UTC()
	record.State = "fulfilled"
	record.Error = ""
	record.NextAttemptAt = time.Time{}
	if err := s.IssueClaims.Save(record); err != nil {
		return err
	}
	s.publishVerifiedIssuePR(record)
	return nil
}

func (s Server) recordMergedUnverifiedIssuePR(record localruntime.IssueClaimRecord, pr issuePullRequest) error {
	record.RunError = record.FailureDetail()
	record.PRURL = pr.URL
	record.PRTitle = strings.TrimSpace(pr.Title)
	record.VerifiedAt = time.Time{}
	record.State = localruntime.IssueStateMergedUnverified
	record.Error = mergedUnverifiedIssueError
	record.NextAttemptAt = time.Time{}
	record.UnlockPending = record.LabelAdded
	return s.IssueClaims.Save(record)
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
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		return err
	}
	if remote.Claim.ID != issue.Claim.ID || remote.InstanceID != s.instanceID() {
		return fmt.Errorf("issue ownership changed; worker cannot continue")
	}
	return nil
}

// Count a terminal outcome once per claim. Each automatic successor carries
// the previous count, including runs that never reached the agent step.
func issueRunFailures(record localruntime.IssueClaimRecord) int {
	if record.Fulfilled() {
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

func issueRetryDelay(id string, failures int, policy localruntime.IssueRecoveryConfig) time.Duration {
	seconds := policy.RetryDelaySeconds
	for i := 1; i < failures && seconds < policy.MaxBackoffSeconds; i++ {
		if seconds > policy.MaxBackoffSeconds/5 {
			seconds = policy.MaxBackoffSeconds
		} else {
			seconds *= 5
		}
	}
	seconds = min(seconds, policy.MaxBackoffSeconds)
	// Stable jitter survives restarts and makes tests deterministic.
	jitter := 0
	if len(id) > 0 {
		jitter = int(id[0]) % max(1, seconds/5)
	}
	return time.Duration(min(seconds+jitter, policy.MaxBackoffSeconds)) * time.Second
}

func (s Server) scheduleIssueRetry(record *localruntime.IssueClaimRecord, cause error) {
	record.Failures++
	record.NextAttemptAt = issueRetryDeadline(record.ID, issueRetryFailures(*record), s.IssueRecovery.Defaults(), cause)
}

func issueRetryDeadline(id string, failures int, policy localruntime.IssueRecoveryConfig, cause error) time.Time {
	deadline := time.Now().Add(issueRetryDelay(id, failures, policy))
	var rate *githubutil.RateLimitError
	if errors.As(cause, &rate) && rate.RetryAt.After(deadline) {
		return rate.RetryAt
	}
	return deadline
}

func (s Server) deferIssueRecord(record localruntime.IssueClaimRecord, cause error) error {
	// Merge, because submission may have launched a worker before returning an
	// ambiguous receipt. Never overwrite a running or completed worker here.
	err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if current.StartedAt.After(record.StartedAt) || current.StoppedAt.After(record.StoppedAt) {
			return nil
		}
		if errors.Is(cause, localruntime.ErrIssueOwnershipChanged) {
			blockStaleIssueClaim(current)
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
			current.NextAttemptAt = issueRetryDeadline(current.ID, issueRetryFailures(*current), s.IssueRecovery.Defaults(), cause)
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
		record.UnlockFailures++
		record.UnlockError = cause.Error()
		record.NextUnlockAt = issueRetryDeadline(record.ID, record.UnlockFailures, s.IssueRecovery.Defaults(), cause)
		return nil
	})
	return errors.Join(cause, err)
}

func (s Server) unlockIssue(ctx context.Context, record *localruntime.IssueClaimRecord) error {
	issue := record.Run.GitHubIssue
	// Read first: deletion after an accepted-but-lost response is idempotent.
	fresh, err := s.fetchGitHubIssue(ctx, issue.Repository, issue.Number)
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
}

func (pr issuePullRequest) mergedUnverified(record localruntime.IssueClaimRecord) bool {
	return pr.URL != "" && pr.Merged && record.PRVerification != ""
}

func (pr issuePullRequest) resumesVerification(record localruntime.IssueClaimRecord) bool {
	return pr.URL != "" && !pr.Closed && !pr.Merged && (record.PRVerification != "" ||
		(record.Outcome == workflow.OutcomeReviewPending && record.PRURL == pr.URL)) &&
		pr.BaseBranch == record.Run.GitHubIssue.Claim.BaseBranch
}

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
		return fmt.Errorf("%w: %s before using Recover / retry", app.ErrIssueBranchRecovery, action)
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
		var prs []struct {
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
		if err := json.Unmarshal([]byte(result.Stdout), &prs); err != nil {
			return issuePullRequest{}, err
		}
		for _, pr := range prs {
			if pr.Number <= 0 || pr.Head.Ref != issue.Claim.Branch ||
				!strings.EqualFold(pr.Head.Repo.FullName, issue.Repository) || !strings.EqualFold(pr.Base.Repo.FullName, issue.Repository) {
				continue
			}
			match := issuePullRequest{
				URL:        fmt.Sprintf("https://github.com/%s/pull/%d", issue.Repository, pr.Number),
				Title:      pr.Title,
				HeadSHA:    pr.Head.SHA,
				BaseBranch: pr.Base.Ref,
				Closed:     pr.State == "closed" && pr.MergedAt == nil,
				Merged:     pr.MergedAt != nil,
			}
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
