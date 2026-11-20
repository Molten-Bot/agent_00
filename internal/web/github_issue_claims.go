package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
)

type issueAutomationState struct {
	mu             sync.Mutex
	admission      sync.Mutex
	status         issueAutomationStatus
	reposRefreshed time.Time
	instanceID     string
}

type issueAutomationStatus struct {
	Enabled      bool                      `json:"enabled"`
	Account      string                    `json:"account"`
	Repositories []string                  `json:"repositories"`
	LastAttempt  time.Time                 `json:"lastAttempt"`
	LastSuccess  time.Time                 `json:"lastSuccess"`
	Error        string                    `json:"error,omitempty"`
	Scanned      int                       `json:"scanned"`
	Skipped      int                       `json:"skipped"`
	Submitted    int                       `json:"submitted"`
	Issues       []issueAutomationDecision `json:"issues"`
}

type issueAutomationDecision struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Reason     string `json:"reason"`
	Error      string `json:"error,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
}

func (status *issueAutomationStatus) addError(err error) {
	if status.Error != "" {
		status.Error += "; "
	}
	status.Error += err.Error()
}

func normalizedIssueSettingsState(state ReviewSettingsState) ReviewSettingsState {
	state.GitHubIssueAutoStartEnabled = state.GitHubIssueAutoStartAssigned
	state.GitHubIssueAutoStartExclusionLabel = issueExclusionLabel(state)
	return state
}

func (req *ReviewSettingsRequest) normalizeIssueAutoStart() error {
	if req.GitHubIssueAutoStartEnabled != nil {
		if req.GitHubIssueAutoStartAssigned != nil && *req.GitHubIssueAutoStartAssigned != *req.GitHubIssueAutoStartEnabled {
			return fmt.Errorf("conflicting issue auto-start settings")
		}
		req.GitHubIssueAutoStartAssigned = req.GitHubIssueAutoStartEnabled
	}
	return nil
}

func issueExclusionLabel(settings ReviewSettingsState) string {
	if label := strings.TrimSpace(settings.GitHubIssueAutoStartExclusionLabel); label != "" {
		return label
	}
	return localruntime.DefaultGitHubIssueExclusionLabel
}

func newIssueClaimID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func (s Server) instanceID() string {
	s.issueAutomation.mu.Lock()
	defer s.issueAutomation.mu.Unlock()
	if s.issueAutomation.instanceID == "" {
		if s.IssueRecovery != nil {
			id, err := s.IssueClaims.Identity()
			if err != nil {
				s.logf("event=issue_identity status=error err=%q", err)
				return ""
			}
			s.issueAutomation.instanceID = id
		} else {
			s.issueAutomation.instanceID = newIssueClaimID()
		}
	}
	return s.issueAutomation.instanceID
}

func (s Server) submitWithIssueClaim(ctx context.Context, body []byte, submit func(context.Context, []byte) (string, error)) (string, error) {
	run, err := localruntime.ParseRunConfigJSON(body)
	if err != nil {
		return submit(ctx, body)
	} // preserve the submitter's validation response
	if run.GitHubIssue == nil {
		return submit(ctx, body)
	}
	if s.ReviewSettingsStatus == nil {
		return "", fmt.Errorf("issue settings are unavailable")
	}
	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		return "", err
	}
	if !settings.GitHubIssuesEnabled {
		return "", fmt.Errorf("GitHub Issues is disabled")
	}
	// Manual starts intentionally need not match automatic trigger filters, but
	// obey exclusions, development associations, and the same ownership protocol.
	settings.GitHubIssueAutoStartOnlyAssigned = false
	settings.GitHubIssueAutoStartLabels = nil
	return s.claimAndSubmitIssueMode(ctx, run, settings, "", submit, true)
}

type issueClaimTarget struct {
	ID         string
	BaseBranch string
	SHA        string
	Linked     int
	ClosingPRs int
	BlockedBy  int
	Branches   []string
}

func (s Server) issueClaimTarget(ctx context.Context, repository string, number int) (issueClaimTarget, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok {
		return issueClaimTarget{}, fmt.Errorf("invalid issue repository")
	}
	const query = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){defaultBranchRef{name target{oid}} issue(number:$number){id linkedBranches(first:100){totalCount nodes{ref{name}}} closedByPullRequestsReferences(first:100,includeClosedPrs:true){totalCount nodes{state merged}} issueDependenciesSummary{blockedBy}}}}`
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + owner, "-f", "name=" + name, "-F", fmt.Sprintf("number=%d", number)}})
	if err != nil {
		return issueClaimTarget{}, err
	}
	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			Repository *struct {
				DefaultBranchRef *struct {
					Name   string
					Target struct{ OID string }
				}
				Issue *struct {
					ID             string
					LinkedBranches struct {
						TotalCount int
						Nodes      []struct{ Ref *struct{ Name string } }
					}
					ClosedByPullRequestsReferences struct {
						TotalCount int
						Nodes      []struct {
							State  string
							Merged bool
						}
					}
					IssueDependenciesSummary struct{ BlockedBy int }
				}
			}
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return issueClaimTarget{}, err
	}
	if len(response.Errors) > 0 {
		return issueClaimTarget{}, fmt.Errorf("load issue development links: %s", response.Errors[0].Message)
	}
	repo := response.Data.Repository
	if repo == nil || repo.Issue == nil || repo.Issue.ID == "" || repo.DefaultBranchRef == nil || repo.DefaultBranchRef.Target.OID == "" {
		return issueClaimTarget{}, fmt.Errorf("incomplete issue or default branch data")
	}
	closing := repo.Issue.ClosedByPullRequestsReferences
	blockingPRs := closing.TotalCount - len(closing.Nodes)
	for _, pr := range closing.Nodes {
		if strings.EqualFold(pr.State, "OPEN") || pr.Merged {
			blockingPRs++
		}
	}
	target := issueClaimTarget{ID: repo.Issue.ID, BaseBranch: repo.DefaultBranchRef.Name, SHA: repo.DefaultBranchRef.Target.OID, Linked: repo.Issue.LinkedBranches.TotalCount, ClosingPRs: blockingPRs, BlockedBy: repo.Issue.IssueDependenciesSummary.BlockedBy}
	for _, node := range repo.Issue.LinkedBranches.Nodes {
		if node.Ref != nil {
			target.Branches = append(target.Branches, node.Ref.Name)
		}
	}
	return target, nil
}

func (s Server) claimAndSubmitIssue(ctx context.Context, run config.Config, settings ReviewSettingsState, login string, submit func(context.Context, []byte) (string, error)) (string, error) {
	return s.claimAndSubmitIssueMode(ctx, run, settings, login, submit, false)
}

var errIssueRecoveryRequired = errors.New("issue is reserved by a previous attempt; use Recover / retry here or in Settings → GitHub Issues after its worker has stopped")

// Only an explicit start/rerun may recover a known failed local attempt. The
// background scanner must never turn a persistent failure into a retry loop.
func (s Server) claimAndSubmitIssueMode(ctx context.Context, run config.Config, settings ReviewSettingsState, login string, submit func(context.Context, []byte) (string, error), manual bool) (string, error) {
	if s.issueAutomation == nil || s.IssueClaims == nil {
		return "", fmt.Errorf("issue claim storage is unavailable")
	}
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()
	issue := run.GitHubIssue
	if issue == nil {
		return "", fmt.Errorf("issue is required")
	}
	if s.activeIssueTask(issue.Repository, issue.Number) {
		return "", fmt.Errorf("issue already has an active local task")
	}
	repos := run.RepoList()
	if len(repos) != 1 || !issueRepositoryMatchesRun(repos[0], issue.Repository) {
		return "", fmt.Errorf("issue tasks require exactly their issue repository")
	}
	for _, disabled := range settings.DisabledRepositories {
		if issueRepositoryMatchesRun(disabled, issue.Repository) {
			return "", fmt.Errorf("repository is disabled")
		}
	}
	records, err := s.IssueClaims.List()
	if err != nil && s.IssueRecovery == nil {
		return "", err
	}
	for _, record := range records {
		prior := record.Run.GitHubIssue
		if strings.EqualFold(prior.Repository, issue.Repository) && prior.Number == issue.Number {
			if manual {
				return s.restartFailedIssue(ctx, issue, records, submit)
			}
			return "", errIssueRecoveryRequired
		}
	}
	if issue.Claim != nil {
		return "", errIssueRecoveryRequired
	}
	if s.IssueRecovery != nil {
		return s.admitDurableIssue(ctx, run, settings, login, submit)
	}
	fresh, err := s.fetchGitHubIssue(ctx, issue.Repository, issue.Number)
	if err != nil {
		return "", err
	}
	if reason := issueEligibilityReason(fresh, settings, login); reason != "" {
		return "", fmt.Errorf("issue is ineligible: %s", reason)
	}
	target, err := s.issueClaimTarget(ctx, issue.Repository, issue.Number)
	if err != nil {
		return "", err
	}
	if target.Linked > 0 || target.ClosingPRs > 0 {
		return "", fmt.Errorf("issue has an associated branch or closing PR")
	}
	if target.BlockedBy > 0 {
		return "", fmt.Errorf("issue is blocked by %d open issue(s)", target.BlockedBy)
	}
	// Use the repository's default base for automatic work; manual selections
	// may choose a different existing base without changing the claim name.
	base := strings.TrimSpace(run.BaseBranch)
	if base == "" {
		base = target.BaseBranch
	}
	sha := target.SHA
	if base != target.BaseBranch {
		sha, err = s.issueBranchSHA(ctx, issue.Repository, base)
		if err != nil {
			return "", err
		}
	}
	claim := &config.GitHubIssueClaim{ID: newIssueClaimID(), Branch: fmt.Sprintf("agent00-issue-%d", issue.Number), BaseBranch: base, ExclusionLabel: issueExclusionLabel(settings), Attempt: 1}
	issue.Claim = claim
	run.BaseBranch = base
	setGitHubIssuePrompt(&run, fresh)
	if err := githubassets.Validate(run.Assets); err != nil {
		return "", err
	}
	record := localruntime.IssueClaimRecord{ID: claim.ID, InstanceID: s.instanceID(), Phase: "reserving", Run: run}
	if record.InstanceID == "" {
		return "", fmt.Errorf("persistent instance identity is unavailable")
	}
	if s.IssueRecovery != nil {
		record.Version = 2
		record.State = "reserving"
		record.IssueNodeID = target.ID
		record.ReservationSHA = sha
	}
	if err := s.IssueClaims.Save(record); err != nil {
		return "", fmt.Errorf("persist claim before reservation: %w", err)
	}
	if record.Version == 2 {
		return s.resumeIssueAdmission(ctx, record, submit)
	}
	if err := s.reserveIssueBranch(ctx, target.ID, claim.Branch, sha); err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	// An immutable annotated tag records each admitted attempt. Its unique ref
	// also serializes explicit recovery across independent instances. It contains
	// ownership metadata only, never issue text, prompts, credentials or logs.
	if err = s.publishIssueAttempt(ctx, issue, sha, record.InstanceID); err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	return s.finishIssueAdmission(ctx, record, submit)
}

func (s Server) reserveIssueBranch(ctx context.Context, issueID, branch, sha string) error {
	const mutation = `mutation($issue:ID!,$name:String!,$oid:GitObjectID!){createLinkedBranch(input:{issueId:$issue,name:$name,oid:$oid}){linkedBranch{ref{name}}}}`
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "graphql", "-f", "query=" + mutation, "-f", "issue=" + issueID, "-f", "name=" + branch, "-f", "oid=" + sha}})
	if err != nil {
		return fmt.Errorf("reserve issue branch (ownership unconfirmed): %w", err)
	}
	var response struct {
		Errors []struct{ Message string }
		Data   struct {
			CreateLinkedBranch struct {
				LinkedBranch struct{ Ref struct{ Name string } }
			}
		}
	}
	if err = json.Unmarshal([]byte(result.Stdout), &response); err != nil || len(response.Errors) > 0 || response.Data.CreateLinkedBranch.LinkedBranch.Ref.Name != branch {
		return fmt.Errorf("GitHub did not confirm exclusive creation of the exact reserved branch; inspect the claim before retrying")
	}
	return nil
}

func (s Server) restartFailedIssue(ctx context.Context, issue *config.GitHubIssueConfig, records []localruntime.IssueClaimRecord, submit func(context.Context, []byte) (string, error)) (string, error) {
	remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
	if err != nil {
		return "", err
	}
	if remote.Claim.ID == "" || remote.InstanceID != s.instanceID() || s.Broker == nil {
		return "", errIssueRecoveryRequired
	}
	if issue.Claim != nil && issue.Claim.ID != remote.Claim.ID {
		return "", errIssueRecoveryRequired
	}
	for _, record := range records {
		prior := record.Run.GitHubIssue
		if record.ID != remote.Claim.ID || record.InstanceID != remote.InstanceID || record.Phase != "submitted" ||
			!strings.EqualFold(prior.Repository, issue.Repository) || prior.Number != issue.Number {
			continue
		}
		task, ok := s.Broker.Task(record.RequestID)
		if !ok {
			break
		}
		switch normalizeTaskTerminalStatus(task.Status) {
		case "error", "invalid", "stopped":
			// Recovery must bypass completed-request deduplication, just like the
			// explicit recovery endpoint. The claim protocol still excludes races.
			if s.SubmitTaskRerun != nil {
				submit = func(ctx context.Context, body []byte) (string, error) {
					return s.SubmitTaskRerun(ctx, record.RequestID, body, false)
				}
			}
			id, _, err := s.recoverIssueClaim(ctx, issue.Repository, issue.Number, remote, submit)
			return id, err
		}
		break
	}
	return "", errIssueRecoveryRequired
}

func (s Server) blockIssueClaim(record localruntime.IssueClaimRecord, cause error) error {
	return errors.Join(cause, s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		current.Phase = "blocked"
		current.Error = cause.Error()
		if current.Version == 2 {
			current.State = "retry_wait"
			current.Incident = "admission"
			current.UnlockPending = current.LabelAdded
		}
		return nil
	}))
}

func (s Server) finishIssueAdmission(ctx context.Context, record localruntime.IssueClaimRecord, submit func(context.Context, []byte) (string, error)) (string, error) {
	issue := record.Run.GitHubIssue
	if record.Version == 2 && s.Broker != nil {
		pending := 0
		for _, task := range s.Broker.TaskList() {
			if !isCompletedTaskStatus(task.Status) {
				pending++
			}
		}
		if pending >= 32 {
			return "", s.blockIssueClaim(record, fmt.Errorf("dispatch capacity reached; issue retained for retry"))
		}
	}
	if record.Version == 2 {
		// Persist label intent before the API call: a lost response still needs
		// reconciliation. Eligibility was checked before acquiring ownership.
		record.LabelAdded = true
		record.State = "queued"
		record.RequestID = "issue-" + record.ID
		if err := s.IssueClaims.Save(record); err != nil {
			return "", err
		}
	}
	if err := s.ensureIssueClaimLabel(ctx, issue); err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	record.Phase = "submitting"
	record.Error = ""
	if err := s.IssueClaims.Save(record); err != nil {
		return "", fmt.Errorf("persist submission intent: %w", err)
	}
	body, err := json.Marshal(record.Run)
	if err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	requestID, err := submit(ctx, body)
	if err != nil {
		return "", s.blockIssueClaim(record, fmt.Errorf("submit claimed issue: %w", err))
	}
	if err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		current.Phase = "submitted"
		current.RequestID = requestID
		return nil
	}); err != nil {
		return "", fmt.Errorf("task %s was accepted but claim receipt could not be saved; do not resubmit: %w", requestID, err)
	}
	s.logf("localruntime.ui status=ok event=github_issue_auto_start issue=%d request_id=%s", issue.Number, requestID)
	return requestID, nil
}

func (s Server) issueBranchSHA(ctx context.Context, repository, branch string) (string, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + repository + "/git/ref/heads/" + branch}})
	if err != nil {
		return "", err
	}
	var ref struct{ Object struct{ SHA string } }
	if err = json.Unmarshal([]byte(result.Stdout), &ref); err != nil {
		return "", err
	}
	if ref.Object.SHA == "" {
		return "", fmt.Errorf("branch has no commit")
	}
	return ref.Object.SHA, nil
}

func (s Server) issueJSON(ctx context.Context, endpoint string, body any) (execx.Result, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return execx.Result{}, err
	}
	return s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "--method", "POST", endpoint, "--input", "-"}, Stdin: string(encoded)})
}

func (s Server) ensureIssueClaimLabel(ctx context.Context, issue *config.GitHubIssueConfig) error {
	label := issue.Claim.ExclusionLabel
	// gh label create --force updates existing labels, which would overwrite the
	// user's color/description. Instead only create missing labels.
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "--paginate", "repos/" + issue.Repository + "/labels?per_page=100"}})
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	found := false
	for decoder.More() {
		var labels []GitHubIssueLabel
		if err := decoder.Decode(&labels); err != nil {
			return err
		}
		for _, entry := range labels {
			if strings.EqualFold(entry.Name, label) {
				found = true
			}
		}
	}
	if !found {
		if _, err = s.issueJSON(ctx, "repos/"+issue.Repository+"/labels", map[string]string{"name": label, "color": "D4C5F9", "description": "Reserved by agent_00; explicit recovery required while open"}); err != nil {
			return fmt.Errorf("create exclusion label: %w", err)
		}
	}
	_, err = s.issueJSON(ctx, fmt.Sprintf("repos/%s/issues/%d/labels", issue.Repository, issue.Number), map[string]any{"labels": []string{label}})
	return err
}

type issueAttemptMetadata struct {
	Claim      config.GitHubIssueClaim `json:"claim"`
	InstanceID string                  `json:"instanceId"`
}

func issueAttemptPrefix(number int) string { return fmt.Sprintf("agent00-issue-%d-claim-", number) }

func (s Server) publishIssueAttempt(ctx context.Context, issue *config.GitHubIssueConfig, sha, instanceID string) error {
	claim := issue.Claim
	name := issueAttemptPrefix(issue.Number) + strconv.Itoa(claim.Attempt)
	message, _ := json.Marshal(issueAttemptMetadata{Claim: *claim, InstanceID: instanceID})
	result, err := s.issueJSON(ctx, "repos/"+issue.Repository+"/git/tags", map[string]string{"tag": name, "message": string(message), "object": sha, "type": "commit"})
	if err != nil {
		return fmt.Errorf("create claim attempt record: %w", err)
	}
	var tag struct{ SHA string }
	if err = json.Unmarshal([]byte(result.Stdout), &tag); err != nil {
		return err
	}
	if tag.SHA == "" {
		return fmt.Errorf("GitHub returned no claim attempt object")
	}
	_, err = s.issueJSON(ctx, "repos/"+issue.Repository+"/git/refs", map[string]string{"ref": "refs/tags/" + name, "sha": tag.SHA})
	if err != nil {
		return fmt.Errorf("reserve claim attempt (may already be owned): %w", err)
	}
	return nil
}

func (s Server) latestIssueAttempt(ctx context.Context, repository string, number int) (issueAttemptMetadata, error) {
	prefix := "refs/tags/" + issueAttemptPrefix(number)
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + repository + "/git/matching-refs/tags/" + issueAttemptPrefix(number)}})
	if err != nil {
		return issueAttemptMetadata{}, err
	}
	var refs []struct {
		Ref    string
		Object struct {
			SHA  string
			Type string
		}
	}
	if err = json.Unmarshal([]byte(result.Stdout), &refs); err != nil {
		return issueAttemptMetadata{}, err
	}
	latest := 0
	sha := ""
	for _, ref := range refs {
		attempt, e := strconv.Atoi(strings.TrimPrefix(ref.Ref, prefix))
		if e == nil && attempt > latest {
			if ref.Object.Type != "tag" {
				return issueAttemptMetadata{}, fmt.Errorf("invalid claim attempt reference")
			}
			latest = attempt
			sha = ref.Object.SHA
		}
	}
	if latest == 0 {
		return issueAttemptMetadata{}, nil
	}
	result, err = s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + repository + "/git/tags/" + sha}})
	if err != nil {
		return issueAttemptMetadata{}, err
	}
	var tag struct{ Message string }
	if err = json.Unmarshal([]byte(result.Stdout), &tag); err != nil {
		return issueAttemptMetadata{}, err
	}
	var metadata issueAttemptMetadata
	if err = json.Unmarshal([]byte(tag.Message), &metadata); err != nil {
		return metadata, err
	}
	if metadata.Claim.Attempt != latest || metadata.Claim.ID == "" || metadata.Claim.Branch != fmt.Sprintf("agent00-issue-%d", number) || metadata.Claim.BaseBranch == "" {
		return metadata, fmt.Errorf("invalid remote claim metadata")
	}
	return metadata, nil
}

func (s Server) handleIssueAutomationStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.issueAutomation == nil {
		writeJSONError(w, 503, fmt.Errorf("issue automation is unavailable"))
		return
	}
	s.issueAutomation.mu.Lock()
	status := s.issueAutomation.status
	s.issueAutomation.mu.Unlock()
	records, err := s.IssueClaims.List()
	// Never expose stored task prompts through the status endpoint.
	claims := []map[string]any{}
	for _, record := range records {
		issue := record.Run.GitHubIssue
		phase := record.Phase
		if record.InstanceID == s.instanceID() && s.Broker != nil && record.RequestID != "" {
			if task, ok := s.Broker.Task(record.RequestID); ok {
				phase = task.Status
			}
		}
		entry := map[string]any{"id": record.ID, "repository": issue.Repository, "number": issue.Number, "phase": phase, "requestId": record.RequestID, "error": record.Error, "instanceId": record.InstanceID, "attempt": issue.Claim.Attempt}
		if record.Version == 2 {
			entry["phase"] = record.State
			entry["step"] = record.Step
			entry["stepStartedAt"] = record.StepStartedAt
			entry["agentAttempts"] = record.AgentAttempts
			entry["nextAttemptAt"] = record.NextAttemptAt
			entry["unlockPending"] = record.UnlockPending
			entry["prUrl"] = record.PRURL
			entry["incident"] = record.Incident
			entry["repairRequestId"] = record.RepairRequestID
			entry["repairError"] = record.RepairError
			lock, lockErr := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(issue))
			if lockErr != nil {
				entry["workerAlive"] = "unknown"
			} else {
				entry["workerAlive"] = lock == nil
				if lock != nil {
					lock.Close()
					if stopped, err := localruntime.IssueProcessesStopped(record); err != nil {
						entry["workerAlive"] = "unknown"
					} else {
						entry["workerAlive"] = !stopped
					}
				}
			}
		} else {
			entry["recoveryNote"] = "legacy claim: worker termination requires explicit confirmation"
		}
		claims = append(claims, entry)
	}
	if err != nil {
		status.addError(err)
	}
	repairs := []map[string]any{}
	if s.IssueRepairs != nil {
		records, repairErr := s.IssueRepairs.List()
		if repairErr != nil {
			status.addError(repairErr)
		}
		for _, record := range records {
			repairs = append(repairs, map[string]any{"requestId": record.RequestID, "state": record.State, "error": record.Error, "prUrl": record.PRURL})
		}
	}
	writeJSON(w, 200, map[string]any{"ok": err == nil, "status": status, "claims": claims, "repairs": repairs})
}

func (s Server) handleIssueAutomationRetry(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if s.issueAutomation == nil || s.SubmitLocalPrompt == nil || s.ReviewSettingsStatus == nil {
		writeJSONError(w, 503, fmt.Errorf("issue recovery is unavailable"))
		return
	}
	var request struct {
		Repository      string `json:"repository"`
		Number          int    `json:"number"`
		ExpectedClaimID string `json:"expectedClaimId"`
		ConfirmStopped  bool   `json:"confirmStopped"`
	}
	if r.Method == http.MethodGet {
		request.Repository = r.URL.Query().Get("repo")
		request.Number, _ = strconv.Atoi(r.URL.Query().Get("number"))
	} else {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeJSONError(w, 400, err)
			return
		}
	}
	repository, ok := validGitHubRepository(request.Repository)
	if !ok || request.Number < 1 {
		writeJSONError(w, 400, fmt.Errorf("valid repository and issue number are required"))
		return
	}
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()
	remote, err := s.latestIssueAttempt(r.Context(), repository, request.Number)
	if err != nil {
		writeJSONError(w, 502, err)
		return
	}
	expectedID := remote.Claim.ID
	if expectedID == "" {
		expectedID = "unrecorded"
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"ok": true, "expectedClaimId": expectedID, "attempt": remote.Claim.Attempt, "instanceId": remote.InstanceID})
		return
	}
	if !request.ConfirmStopped {
		writeJSONError(w, 409, fmt.Errorf("confirm that the previous worker has stopped before recovering a claim"))
		return
	}
	if expectedID != request.ExpectedClaimID {
		writeJSONError(w, 409, fmt.Errorf("claim changed; refresh before retrying"))
		return
	}
	submit := s.issueRetrySubmitter()
	requestID, status, err := s.recoverIssueClaim(r.Context(), repository, request.Number, remote, submit)
	if err != nil {
		writeJSONError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "request_id": requestID})
}

func (s Server) issueRetrySubmitter() func(context.Context, []byte) (string, error) {
	if s.SubmitTaskRerun != nil {
		return func(ctx context.Context, body []byte) (string, error) { return s.SubmitTaskRerun(ctx, "", body, false) }
	}
	return s.SubmitLocalPrompt
}

// The caller holds admission and has verified that the prior worker stopped.
func (s Server) recoverIssueClaim(ctx context.Context, repository string, number int, remote issueAttemptMetadata, submit func(context.Context, []byte) (string, error)) (string, int, error) {
	return s.recoverIssueClaimMode(ctx, repository, number, remote, submit, false)
}

func (s Server) recoverIssueClaimMode(ctx context.Context, repository string, number int, remote issueAttemptMetadata, submit func(context.Context, []byte) (string, error), automatic bool) (string, int, error) {
	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		return "", 503, err
	}
	if !settings.GitHubIssuesEnabled {
		return "", 409, fmt.Errorf("GitHub Issues is disabled")
	}
	for _, disabled := range settings.DisabledRepositories {
		if issueRepositoryMatchesRun(disabled, repository) {
			return "", 409, fmt.Errorf("repository is disabled")
		}
	}
	records, err := s.IssueClaims.List()
	if err != nil && s.IssueRecovery == nil {
		return "", 503, err
	}
	if s.IssueRecovery != nil {
		lock, err := s.IssueClaims.TryLock(localruntime.IssueExecutionKey(&config.GitHubIssueConfig{Repository: repository, Number: number}))
		if err != nil {
			return "", 503, err
		}
		if lock == nil {
			return "", 409, fmt.Errorf("stop the active worker and its subprocesses before retrying")
		}
		lock.Close()
	}
	if s.IssueRecovery == nil && s.activeIssueTask(repository, number) {
		return "", http.StatusConflict, fmt.Errorf("stop the active task before retrying")
	}
	var savedRun *config.Config
	var previous localruntime.IssueClaimRecord
	for _, record := range records {
		issue := record.Run.GitHubIssue
		if !strings.EqualFold(issue.Repository, repository) || issue.Number != number {
			continue
		}
		if record.Version == 2 && record.InstanceID == s.instanceID() {
			stopped, err := localruntime.IssueProcessesStopped(record)
			if !stopped && (err == nil || automatic) {
				return "", 409, fmt.Errorf("worker termination is not confirmed; inspect subprocesses before retrying")
			}
		}
		if record.Version != 2 && record.InstanceID == s.instanceID() && record.RequestID != "" && s.Broker != nil {
			if task, ok := s.Broker.Task(record.RequestID); ok && !isCompletedTaskStatus(task.Status) {
				return "", 409, fmt.Errorf("stop the active task before retrying")
			}
		}
		if issue.Claim.ID == remote.Claim.ID || remote.Claim.ID == "" {
			run := record.Run
			savedRun = &run
			previous = record
		}
	}
	if s.IssueRecovery != nil && remote.Claim.ID != "" {
		pr, err := s.verifiedIssuePR(ctx, &config.GitHubIssueConfig{Repository: repository, Number: number, Claim: &remote.Claim})
		if err != nil {
			return "", 502, err
		}
		if pr != "" {
			return "", 409, fmt.Errorf("issue already has an open or merged PR")
		}
	}
	issue, err := s.fetchGitHubIssue(ctx, repository, number)
	if err != nil {
		return "", 502, err
	}
	if !strings.EqualFold(issue.State, "open") {
		return "", 409, fmt.Errorf("closed issues cannot be retried")
	}
	target, err := s.issueClaimTarget(ctx, repository, number)
	if err != nil {
		return "", 502, err
	}
	if target.BlockedBy > 0 {
		return "", 409, fmt.Errorf("issue is blocked by %d open issue(s)", target.BlockedBy)
	}
	branch := fmt.Sprintf("agent00-issue-%d", number)
	linked := false
	for _, name := range target.Branches {
		if name == branch {
			linked = true
		}
	}
	// Version two publishes ownership before creating the linked branch. A
	// failed branch reservation can therefore have a remote attempt already.
	// Only the matching local intent may recover that incomplete admission;
	// never recreate a previously confirmed branch that has since disappeared.
	incompleteReservation := remote.Claim.ID == "" || (previous.Version == 2 &&
		previous.ID == remote.Claim.ID && previous.InstanceID == remote.InstanceID &&
		previous.InstanceID == s.instanceID() && !previous.BranchReserved)
	reserve := target.Linked == 0 && target.ClosingPRs == 0 && savedRun != nil && incompleteReservation
	if !reserve && (!linked || target.Linked != 1) {
		return "", 409, fmt.Errorf("recovery requires the single reserved issue branch; inspect development links first")
	}
	base := remote.Claim.BaseBranch
	if base == "" && savedRun != nil {
		base = savedRun.GitHubIssue.Claim.BaseBranch
	}
	if base == "" {
		base = target.BaseBranch
	}
	run := issueRunConfig(GitHubRepo{FullName: repository, DefaultBranch: base}, issue, settings.GitHubIssueLinkMode)
	if savedRun != nil {
		run = *savedRun
	}
	exclusion := remote.Claim.ExclusionLabel
	if exclusion == "" && savedRun != nil {
		exclusion = savedRun.GitHubIssue.Claim.ExclusionLabel
	}
	if exclusion == "" {
		exclusion = issueExclusionLabel(settings)
	}
	if automatic && !previous.LabelAdded && githubIssueHasAnyLabel(issue, []string{exclusion}) {
		return "", 409, fmt.Errorf("issue was excluded after its worker stopped")
	}
	claim := &config.GitHubIssueClaim{ID: newIssueClaimID(), Branch: branch, BaseBranch: base, ExclusionLabel: exclusion, Attempt: remote.Claim.Attempt + 1}
	// Copy rather than mutating a stored config's issue pointer.
	runIssue := *run.GitHubIssue
	run.GitHubIssue = &runIssue
	runIssue.Claim = claim
	run.BaseBranch = base
	setGitHubIssuePrompt(&run, issue)
	if err := githubassets.Validate(run.Assets); err != nil {
		return "", 400, err
	}
	record := localruntime.IssueClaimRecord{ID: claim.ID, InstanceID: s.instanceID(), Phase: "reserving", Run: run}
	if s.IssueRecovery != nil {
		if record.InstanceID == "" {
			return "", 503, fmt.Errorf("persistent instance identity is unavailable")
		}
		record.Version = 2
		record.State = "reserving"
		record.IssueNodeID = target.ID
		record.BranchReserved = !reserve
		record.WorkspaceDir = previous.WorkspaceDir
		if automatic {
			record.AgentAttempts = previous.AgentAttempts
		}
	}
	if err = s.IssueClaims.Save(record); err != nil {
		return "", 503, err
	}
	var sha string
	if record.Version == 2 {
		ref := branch
		if reserve {
			ref = base
		}
		sha, err = s.issueBranchSHA(ctx, repository, ref)
		if err != nil {
			return "", 502, s.blockIssueClaim(record, err)
		}
		record.ReservationSHA = sha
		if err := s.IssueClaims.Save(record); err != nil {
			return "", 503, err
		}
		requestID, err := s.resumeIssueAdmission(ctx, record, submit)
		if err != nil {
			return "", 409, err
		}
		return requestID, http.StatusAccepted, nil
	}
	if reserve {
		sha, err = s.issueBranchSHA(ctx, repository, base)
		if err == nil {
			err = s.reserveIssueBranch(ctx, target.ID, branch, sha)
		}
	} else {
		sha, err = s.issueBranchSHA(ctx, repository, branch)
	}
	if err == nil {
		err = s.publishIssueAttempt(ctx, run.GitHubIssue, sha, record.InstanceID)
	}
	if err != nil {
		return "", 409, s.blockIssueClaim(record, err)
	}
	requestID, err := s.finishIssueAdmission(ctx, record, submit)
	if err != nil {
		return "", http.StatusConflict, err
	}
	return requestID, http.StatusAccepted, nil
}

func issueRepositoryMatchesRun(value, repository string) bool {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	return strings.EqualFold(value, repository)
}

// Inspect the broker as well as durable receipts: enqueue may have succeeded
// immediately before saving its request ID failed.
func (s Server) activeIssueTask(repository string, number int) bool {
	if s.Broker == nil {
		return false
	}
	for _, task := range s.Broker.TaskList() {
		if isCompletedTaskStatus(task.Status) {
			continue
		}
		body, ok := s.Broker.TaskRunConfig(task.RequestID)
		if !ok {
			continue
		}
		var run struct {
			GitHubIssue *config.GitHubIssueConfig `json:"githubIssue"`
		}
		if json.Unmarshal(body, &run) == nil && run.GitHubIssue != nil && run.GitHubIssue.Number == number && strings.EqualFold(run.GitHubIssue.Repository, repository) {
			return true
		}
	}
	return false
}
