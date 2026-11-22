package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/failurefollowup"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

const (
	unmergedDependencyIncident = "unmerged_dependency"
	openDependencyIncident     = "open_dependency"
)

func issueDependencyRetryError(incident string) string {
	switch incident {
	case unmergedDependencyIncident:
		return failurefollowup.UnmergedDependencyReason + "; explicit retry required after merge"
	case openDependencyIncident:
		return failurefollowup.OpenDependencyReason + "; explicit retry required after resolution"
	default:
		return ""
	}
}

func (s Server) WakeIssueRecovery() { s.wakeGitHubIssueAutoStart() }

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
		return record, fmt.Errorf("issue is ineligible: %s", reason)
	}
	target, err := s.issueClaimTarget(ctx, issue.Repository, issue.Number)
	if err != nil {
		return record, err
	}
	if target.Linked > 0 || target.ClosingPRs > 0 {
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

// ReconcileIssueWork is also invoked after worker teardown. It does not rely on
// broker terminal status, labels, or elapsed time to establish worker liveness.
func (s Server) ReconcileIssueWork(ctx context.Context) error {
	if s.IssueRecovery == nil || s.IssueClaims == nil || s.issueAutomation == nil {
		return nil
	}
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()
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
	if record.State == "fulfilled" && !record.UnlockPending {
		return nil
	}
	lock, err := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(record.Run.GitHubIssue))
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
	// Preserve the classification before later checks can replace the last
	// error. This also handles records written before the incident existed.
	if record.Outcome == "error" && issueDependencyRetryError(record.Incident) == "" {
		switch failurefollowup.NonRemediableFailureReason(errors.New(record.FailureDetail())) {
		case failurefollowup.UnmergedDependencyReason:
			record.Incident = unmergedDependencyIncident
		case failurefollowup.OpenDependencyReason:
			record.Incident = openDependencyIncident
		}
		if issueDependencyRetryError(record.Incident) != "" {
			record.NextAttemptAt = time.Time{}
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
	if !record.NextAttemptAt.IsZero() && time.Now().Before(record.NextAttemptAt) {
		return nil
	}
	issue := record.Run.GitHubIssue
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		return s.deferIssueRecord(record, err)
	}
	if staleOwner && (remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID) {
		return nil
	}
	resumingNewIntent := record.Outcome == "" && !record.RemoteRecorded && remote.Claim.Attempt < issue.Claim.Attempt
	if remote.Claim.ID != "" && !resumingNewIntent && (remote.Claim.ID != record.ID || remote.InstanceID != record.InstanceID) {
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
	// Unlock must remain possible even if PR lookup or settings are unavailable.
	if record.UnlockPending && remote.Claim.ID == record.ID {
		if err := s.unlockIssue(ctx, &record); err != nil {
			return s.deferIssueRecord(record, err)
		}
	}
	if remote.Claim.ID == record.ID && record.BranchReserved {
		pr, err := s.verifiedIssuePR(ctx, issue)
		if err != nil {
			return s.deferIssueRecord(record, err)
		}
		if pr != "" {
			record.PRURL = pr
			record.VerifiedAt = time.Now().UTC()
			record.State = "fulfilled"
			record.Error = ""
			record.NextAttemptAt = time.Time{}
			return s.IssueClaims.Save(record)
		}
	}
	// A restored owner can finalize a stopped attempt. Dispatch still requires
	// an explicit retry.
	if staleOwner {
		return s.IssueClaims.Save(record)
	}
	if record.Outcome != "" && record.Incident == "" {
		record.Incident = "missing_pr"
	}
	if s.IssueRepair != nil && record.RepairRequestID == "" && repairableIssueIncident(record) {
		id, repairErr := s.IssueRepair(ctx, record)
		if repairErr != nil {
			record.RepairError = repairErr.Error()
		} else {
			record.RepairRequestID = id
			record.RepairError = ""
		}
	}
	policy := s.IssueRecovery.Defaults()
	dependencyRetryError := issueDependencyRetryError(record.Incident)
	if record.Control != "" || record.Outcome == "stopped" || record.Outcome == "no_changes" ||
		dependencyRetryError != "" || record.AgentAttempts >= policy.MaxAttempts {
		record.State = "blocked"
		switch {
		case record.Outcome == "stopped":
			record.Error = "stopped by operator; explicit resume required"
		case record.Outcome == "no_changes":
			record.Error = "no PR produced; review no-change evidence before retrying"
		case dependencyRetryError != "":
			record.Error = dependencyRetryError
			record.NextAttemptAt = time.Time{}
		default:
			record.Error = "agent retry budget exhausted; explicit retry or deployed fix required"
		}
		if record.Control != "" {
			record.Error = "operator " + record.Control + "; explicit resume required"
		}
		return s.IssueClaims.Save(record)
	}
	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		return s.deferIssueRecord(record, err)
	}
	settings, autoStart := settings.issueAutoStartForRepository(issue.Repository)
	if !autoStart {
		record.State = "blocked"
		record.Error = "issue automation disabled"
		return s.IssueClaims.Save(record)
	}
	for _, disabled := range settings.DisabledRepositories {
		if issueRepositoryMatchesRun(disabled, issue.Repository) {
			record.State = "blocked"
			record.Error = "repository disabled"
			return s.IssueClaims.Save(record)
		}
	}
	if !record.NextAttemptAt.IsZero() && time.Now().Before(record.NextAttemptAt) {
		return s.IssueClaims.Save(record)
	}
	if record.Outcome != "" && record.NextAttemptAt.IsZero() {
		record.State = "retry_wait"
		record.NextAttemptAt = time.Now().Add(issueRetryDelay(record, policy))
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
			login, err := s.issueGitHubLogin(ctx)
			if err != nil {
				return s.deferIssueRecord(record, err)
			}
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
		_, _, err = s.recoverIssueClaimMode(ctx, issue.Repository, issue.Number, remote, s.issueRetrySubmitter(), true)
	}
	if err != nil {
		return s.deferIssueRecord(record, err)
	}
	return nil
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

func issueRetryDelay(record localruntime.IssueClaimRecord, policy localruntime.IssueRecoveryConfig) time.Duration {
	seconds := policy.RetryDelaySeconds
	for i := 1; i < max(record.AgentAttempts, record.Failures) && seconds < policy.MaxBackoffSeconds; i++ {
		if seconds > policy.MaxBackoffSeconds/5 {
			seconds = policy.MaxBackoffSeconds
		} else {
			seconds *= 5
		}
	}
	seconds = min(seconds, policy.MaxBackoffSeconds)
	// Stable jitter survives restarts and makes tests deterministic.
	jitter := 0
	if len(record.ID) > 0 {
		jitter = int(record.ID[0]) % max(1, seconds/5)
	}
	return time.Duration(min(seconds+jitter, policy.MaxBackoffSeconds)) * time.Second
}

func repairableIssueIncident(record localruntime.IssueClaimRecord) bool {
	if record.Control != "" || record.Incident == "" || record.Incident == "service_shutdown" ||
		issueDependencyRetryError(record.Incident) != "" || record.Outcome == "stopped" || (record.Outcome == "no_changes" && record.NoChangeEvidence) {
		return false
	}
	if record.Outcome == "" && record.Failures < 3 {
		return false
	}
	detail := record.FailureDetail()
	if failurefollowup.NonRemediableFailureReason(errors.New(detail)) != "" {
		return false
	}
	if strings.Contains(detail, localruntime.LegacyIssueOwnershipFailure) {
		return false
	}
	lower := strings.ToLower(detail)
	for _, reason := range []string{"issue is ineligible", "associated branch", "open dependencies", "dispatch capacity", "automation disabled", "repository disabled", "ownership", "permission", "rate limit", "worker descendants", "agent auth required"} {
		if strings.Contains(lower, reason) {
			return false
		}
	}
	return true
}

func (s Server) deferIssueRecord(record localruntime.IssueClaimRecord, cause error) error {
	// Merge, because submission may have launched a worker before returning an
	// ambiguous receipt. Never overwrite a running or completed worker here.
	err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if current.StartedAt.After(record.StartedAt) || current.StoppedAt.After(record.StoppedAt) {
			return nil
		}
		if record.RepairRequestID != "" {
			current.RepairRequestID = record.RepairRequestID
		}
		current.RepairError = record.RepairError
		if errors.Is(cause, localruntime.ErrIssueOwnershipChanged) {
			blockStaleIssueClaim(current)
			return nil
		}
		if !isStaleIssueClaim(*current) {
			current.State = "retry_wait"
			current.Error = cause.Error()
		}
		current.Failures++
		current.NextAttemptAt = time.Now().Add(issueRetryDelay(*current, s.IssueRecovery.Defaults()))
		var rate *issueRateLimitError
		if errors.As(cause, &rate) && rate.RetryAt.After(current.NextAttemptAt) {
			current.NextAttemptAt = rate.RetryAt
		}
		return nil
	})
	return errors.Join(cause, err)
}

func isStaleIssueClaim(record localruntime.IssueClaimRecord) bool {
	return record.State == "blocked" && record.Error == localruntime.ErrIssueOwnershipChanged.Error()
}

func blockStaleIssueClaim(record *localruntime.IssueClaimRecord) {
	record.State = "blocked"
	record.Error = localruntime.ErrIssueOwnershipChanged.Error()
	record.NextAttemptAt = time.Time{}
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
	return s.IssueClaims.Save(*record)
}

func (s Server) verifiedIssuePR(ctx context.Context, issue *config.GitHubIssueConfig) (string, error) {
	owner, _, _ := strings.Cut(issue.Repository, "/")
	for page := 1; ; page++ {
		query := url.Values{"state": {"all"}, "head": {owner + ":" + issue.Claim.Branch}, "base": {issue.Claim.BaseBranch}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + issue.Repository + "/pulls?" + query.Encode()}})
		if err != nil {
			return "", err
		}
		var prs []struct {
			Number   int
			State    string
			MergedAt *time.Time `json:"merged_at"`
			Head     struct {
				Ref  string
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
			return "", err
		}
		for _, pr := range prs {
			if pr.Number > 0 && (pr.State == "open" || pr.MergedAt != nil) && pr.Head.Ref == issue.Claim.Branch && pr.Base.Ref == issue.Claim.BaseBranch && strings.EqualFold(pr.Head.Repo.FullName, issue.Repository) && strings.EqualFold(pr.Base.Repo.FullName, issue.Repository) {
				return fmt.Sprintf("https://github.com/%s/pull/%d", issue.Repository, pr.Number), nil
			}
		}
		if len(prs) < 100 {
			return "", nil
		}
	}
}
