package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/app"
	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workflow"
)

type issueAutomationState struct {
	mu         sync.Mutex
	admission  sync.Mutex
	status     issueAutomationStatus
	instanceID string
}

type issueAutomationStatus struct {
	RetryAt      *time.Time                `json:"retryAt,omitempty"`
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
	// Durable admission and execution must use the same on-disk owner. A
	// cached legacy identity (or an identity from replaced storage) must not
	// poison every subsequent explicit recovery attempt.
	if s.IssueRecovery != nil {
		id, err := s.IssueClaims.Identity()
		if err != nil {
			s.logf("event=issue_identity status=error err=%q", err)
			return ""
		}
		return id
	}
	if s.issueAutomation.instanceID == "" {
		s.issueAutomation.instanceID = newIssueClaimID()
	}
	return s.issueAutomation.instanceID
}

func (s Server) submitWithIssueClaim(ctx context.Context, body []byte, submit func(context.Context, []byte) (string, error)) (string, error) {
	run, err := localruntime.ParseRunConfigJSON(body)
	if err != nil {
		return submit(ctx, body)
	} // preserve the submitter's validation response
	return s.submitParsedWithIssueClaim(ctx, body, run, submit)
}

func (s Server) submitParsedWithIssueClaim(ctx context.Context, body []byte, run config.Config, submit func(context.Context, []byte) (string, error)) (string, error) {
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
	if !settings.GitHubIssuesEnabled && (run.Workflow == nil || run.GitHubIssue.Claim == nil) {
		return "", fmt.Errorf("GitHub Issues is disabled")
	}
	// Explicit starts use the issue as the execution key; automatic trigger
	// filters and development links do not establish another worker.
	settings.GitHubIssueAutoStartOnlyAssigned = false
	settings.GitHubIssueAutoStartLabels = nil
	return s.claimAndSubmitIssueMode(ctx, run, settings, "", submit, true)
}

type issueClaimTarget struct {
	ID               string
	BaseBranch       string
	SHA              string
	Linked           int
	ClosingPRs       int
	OnlyClosingPRURL string
	BlockedBy        int
	Branches         []string
}

func (s Server) issueClaimTarget(ctx context.Context, repository string, number int) (issueClaimTarget, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok {
		return issueClaimTarget{}, fmt.Errorf("invalid issue repository")
	}
	const query = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){defaultBranchRef{name target{oid}} issue(number:$number){id linkedBranches(first:100){totalCount nodes{ref{name}}} closedByPullRequestsReferences(first:100,includeClosedPrs:true){totalCount nodes{url state merged}} issueDependenciesSummary{blockedBy}}}}`
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
							URL    string
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
	var closingPRURL string
	for _, pr := range closing.Nodes {
		if strings.EqualFold(pr.State, "OPEN") || pr.Merged {
			blockingPRs++
			closingPRURL = pr.URL
		}
	}
	target := issueClaimTarget{ID: repo.Issue.ID, BaseBranch: repo.DefaultBranchRef.Name, SHA: repo.DefaultBranchRef.Target.OID, Linked: repo.Issue.LinkedBranches.TotalCount, ClosingPRs: blockingPRs, BlockedBy: repo.Issue.IssueDependenciesSummary.BlockedBy}
	if blockingPRs == 1 {
		target.OnlyClosingPRURL = closingPRURL
	}
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

// Explicit starts recover previous attempts under the one-worker-per-issue assumption.
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
	if manual {
		remote, err := s.latestIssueAttempt(ctx, issue.Repository, issue.Number)
		if err != nil {
			return "", err
		}
		result, _, err := s.recoverIssueClaimWithRun(ctx, issue.Repository, issue.Number, remote, submit, false, &run)
		if result.PRURL != "" {
			return "", fmt.Errorf("issue already fulfilled by its verified pull request")
		}
		return result.RequestID, err
	}
	records, err := s.IssueClaims.List()
	if err != nil && s.IssueRecovery == nil {
		return "", err
	}
	for _, record := range records {
		prior := record.Run.GitHubIssue
		if strings.EqualFold(prior.Repository, issue.Repository) && prior.Number == issue.Number {
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

// Admission can fail after a successor claim is saved. Tell reconciliation
// that the failed intent already owns the retry deadline.
type issueAdmissionDeferredError struct{ error }

func (e *issueAdmissionDeferredError) Unwrap() error { return e.error }

func (s Server) blockIssueClaim(record localruntime.IssueClaimRecord, cause error) error {
	err := s.IssueClaims.Update(record.ID, func(current *localruntime.IssueClaimRecord) error {
		if current.StartedAt.After(record.StartedAt) || current.StoppedAt.After(record.StoppedAt) {
			return nil
		}
		current.Phase = "blocked"
		current.Error = cause.Error()
		if current.Version == 2 {
			current.State = "retry_wait"
			current.Incident = "admission"
			current.UnlockPending = current.LabelAdded
			if errors.Is(cause, localruntime.ErrIssueOwnershipChanged) {
				blockStaleIssueClaim(current)
			} else if errors.Is(cause, errWorkflowIssueRecovery) {
				blockObservedWorkflowIssue(current)
			} else if errors.Is(cause, app.ErrIssueBranchRecovery) {
				blockIssueBranchRecovery(current, cause.Error())
			} else if s.IssueRecovery != nil {
				s.scheduleIssueRetry(current, cause)
				var rate *githubutil.RateLimitError
				if current.UnlockPending && errors.As(cause, &rate) && rate.RetryAt.After(current.NextUnlockAt) {
					current.NextUnlockAt = rate.RetryAt
				}
			}
		}
		return nil
	})
	if err == nil && record.Version == 2 && s.IssueRecovery != nil {
		return &issueAdmissionDeferredError{cause}
	}
	return errors.Join(cause, err)
}

func (s Server) finishIssueAdmission(ctx context.Context, record localruntime.IssueClaimRecord, submit func(context.Context, []byte) (string, error)) (string, error) {
	issue := record.Run.GitHubIssue
	if record.Version == 2 {
		if err := s.IssueClaims.CheckOwner(record.InstanceID); err != nil {
			return "", s.blockIssueClaim(record, err)
		}
	}
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
		if record.RequestID == "" {
			record.RequestID = "issue-" + record.ID
		}
		if err := s.IssueClaims.Save(record); err != nil {
			return "", err
		}
	}
	if err := s.ensureIssueClaimLabel(ctx, issue); err != nil {
		return "", s.blockIssueClaim(record, err)
	}
	if record.Version == 2 && record.BranchReserved {
		exists, err := s.issueBranchExists(ctx, issue.Repository, issue.Claim.Branch)
		if err != nil {
			return "", s.blockIssueClaim(record, err)
		}
		if !exists {
			return "", s.blockIssueClaim(record, missingReservedIssueRef(issue.Claim.Branch))
		}
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
	if record.RequireReservationSHA {
		sha, err := s.issueBranchSHA(ctx, issue.Repository, issue.Claim.Branch)
		if err != nil {
			return "", s.blockIssueClaim(record, err)
		}
		if sha != record.ReservationSHA {
			return "", s.blockIssueClaim(record, fmt.Errorf("%w: branch %q moved after verification; inspect its commits before using Recover / retry", app.ErrIssueBranchRecovery, issue.Claim.Branch))
		}
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
	s.logf("localruntime.ui status=ok event=github_issue_submitted issue=%d request_id=%s", issue.Number, requestID)
	return requestID, nil
}

// Matching refs returns an empty list for a missing branch without conflating
// absence with authentication failures or transient GitHub errors.
func (s Server) issueBranchExists(ctx context.Context, repository, branch string) (bool, error) {
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + repository + "/git/matching-refs/heads/" + branch}})
	if err != nil {
		return false, err
	}
	var refs []struct{ Ref string }
	if err := json.Unmarshal([]byte(result.Stdout), &refs); err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.Ref == "refs/heads/"+branch {
			return true, nil
		}
	}
	return false, nil
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
		return issueAttemptMetadata{}, fmt.Errorf("read issue claim references: %w: %s", err, strings.TrimSpace(result.Stderr))
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
		return issueAttemptMetadata{}, fmt.Errorf("read issue claim tag: %w: %s", err, strings.TrimSpace(result.Stderr))
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
	if until := s.GitHubCooldown.RetryAt(); !until.IsZero() {
		status.RetryAt = &until
	}
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
			entry["runFailures"] = issueRunFailures(record)
			entry["recoveryFailures"] = record.Failures
			entry["nextAttemptAt"] = record.NextAttemptAt
			entry["nextUnlockAt"] = record.NextUnlockAt
			entry["unlockPending"] = record.UnlockPending
			entry["unlockError"] = record.UnlockError
			entry["prUrl"] = record.PRURL
			entry["incident"] = record.Incident
			entry["repairRequestId"] = record.RepairRequestID
			entry["repairError"] = record.RepairError
			lock, lockErr := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(issue))
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
	allowed, err := s.RepositoryAllowed(r.Context(), repository)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}
	if !allowed {
		writeJSONError(w, http.StatusForbidden, fmt.Errorf("repository is disabled in Settings"))
		return
	}
	s.issueAutomation.admission.Lock()
	defer s.issueAutomation.admission.Unlock()
	remote, err := s.latestIssueAttempt(r.Context(), repository, request.Number)
	if err != nil {
		writeJSONError(w, githubErrorStatus(w, http.StatusBadGateway, err), err)
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
	result, status, err := s.recoverIssueClaimWithRun(r.Context(), repository, request.Number, remote, submit, false, nil)
	if err != nil {
		writeJSONError(w, githubErrorStatus(w, status, err), err)
		return
	}
	writeJSON(w, status, struct {
		OK bool `json:"ok"`
		issueRetryResult
	}{OK: true, issueRetryResult: result})
}

type issueRetryResult struct {
	RequestID string `json:"request_id,omitempty"`
	PRURL     string `json:"prUrl,omitempty"`
}

func (s Server) issueRetrySubmitter() func(context.Context, []byte) (string, error) {
	if s.SubmitTaskRerun != nil {
		return func(ctx context.Context, body []byte) (string, error) { return s.SubmitTaskRerun(ctx, "", body, false) }
	}
	return s.SubmitLocalPrompt
}

// The caller holds admission. Recovery checks local worker liveness.
func (s Server) recoverIssueClaimWithRun(ctx context.Context, repository string, number int, remote issueAttemptMetadata, submit func(context.Context, []byte) (string, error), automatic bool, requestedRun *config.Config) (issueRetryResult, int, error) {
	settings, err := s.ReviewSettingsStatus(ctx)
	if err != nil {
		return issueRetryResult{}, 503, err
	}
	for _, disabled := range settings.DisabledRepositories {
		if issueRepositoryMatchesRun(disabled, repository) {
			return issueRetryResult{}, 409, fmt.Errorf("repository is disabled")
		}
	}
	records, err := s.IssueClaims.List()
	if err != nil && (!automatic || s.IssueRecovery == nil) {
		return issueRetryResult{}, 503, err
	}
	if s.IssueRecovery != nil {
		lock, err := s.IssueClaims.TryControlLock(localruntime.IssueExecutionKey(&config.GitHubIssueConfig{Repository: repository, Number: number}))
		if err != nil {
			return issueRetryResult{}, 503, err
		}
		if lock == nil {
			return issueRetryResult{}, 409, fmt.Errorf("stop the active worker and its subprocesses before retrying")
		}
		lock.Close()
	}
	if s.activeIssueTask(repository, number) {
		return issueRetryResult{}, http.StatusConflict, fmt.Errorf("stop the active task before retrying")
	}
	var savedRun *config.Config
	var previous localruntime.IssueClaimRecord
	var observed localruntime.IssueClaimRecord
	for _, record := range records {
		issue := record.Run.GitHubIssue
		if !strings.EqualFold(issue.Repository, repository) || issue.Number != number {
			continue
		}
		if record.Version == 2 && record.InstanceID == s.instanceID() {
			stopped, err := localruntime.IssueProcessesStopped(record)
			if !stopped && (err == nil || automatic) {
				return issueRetryResult{}, 409, fmt.Errorf("worker termination is not confirmed; inspect subprocesses before retrying")
			}
		}
		if record.Version != 2 && record.InstanceID == s.instanceID() && record.RequestID != "" && s.Broker != nil {
			if task, ok := s.Broker.Task(record.RequestID); ok && !isCompletedTaskStatus(task.Status) {
				return issueRetryResult{}, 409, fmt.Errorf("stop the active task before retrying")
			}
		}
		if issue.Claim.ID == remote.Claim.ID || remote.Claim.ID == "" {
			run := record.Run
			savedRun = &run
			previous = record
		}
		if isUnadmittedWorkflowIssue(record) && record.UpdatedAt.After(observed.UpdatedAt) {
			observed = record
		}
	}
	workflowRecovery := !automatic && remote.Claim.ID != "" &&
		((savedRun == nil && observed.ID != "") || hasUnsubmittedWorkflowRecovery(previous))
	metadataRetry := app.IsWorkflowPRMetadataFailure(previous.FailureDetail()) ||
		(savedRun != nil && savedRun.Workflow != nil && savedRun.Workflow.ResumePRURL != "" &&
			previous.PRVerification == "" && previous.Outcome != workflow.OutcomeReviewPending)
	ownedWorkflowRetry := !automatic && !workflowRecovery && (requestedRun == nil || metadataRetry) && previous.Version == 2 && previous.RequestID != "" &&
		previous.InstanceID == remote.InstanceID && previous.InstanceID == s.instanceID() &&
		previous.ID == remote.Claim.ID && savedRun != nil && savedRun.Workflow != nil
	if !automatic && !workflowRecovery && !ownedWorkflowRetry && !settings.GitHubIssuesEnabled {
		return issueRetryResult{}, 409, fmt.Errorf("GitHub Issues is disabled")
	}
	if workflowRecovery {
		if requestedRun != nil {
			return issueRetryResult{}, http.StatusConflict, errIssueRecoveryRequired
		}
		if savedRun == nil {
			savedRun = &observed.Run
		}
	}
	if metadataRetry && !automatic && savedRun != nil && savedRun.Workflow != nil && s.Workflows != nil {
		policy, policyErr := s.Workflows.PRMetadataRetryPolicy(*savedRun.Workflow)
		if policyErr != nil {
			return issueRetryResult{}, http.StatusConflict, policyErr
		}
		savedRun.Workflow = &policy
	}
	if workflowRecovery || ownedWorkflowRetry {
		var enabled bool
		_, enabled, err = s.workflowIssueSettings(*savedRun)
		if err != nil {
			return issueRetryResult{}, http.StatusServiceUnavailable, err
		}
		if !enabled {
			return issueRetryResult{}, http.StatusConflict, fmt.Errorf("recovery requires the originating workflow to be enabled with its original policy")
		}
	}
	if automatic {
		if savedRun == nil {
			return issueRetryResult{}, 409, fmt.Errorf("automatic issue retry requires a saved workflow run")
		}
		var enabled bool
		settings, enabled, err = s.workflowIssueSettings(*savedRun)
		if err != nil {
			return issueRetryResult{}, 503, err
		}
		if !enabled {
			return issueRetryResult{}, 409, fmt.Errorf("automatic issue retry requires an enabled workflow")
		}
	}
	if requestedRun != nil && s.SubmitTaskRerun != nil {
		submit = func(ctx context.Context, body []byte) (string, error) {
			return s.SubmitTaskRerun(ctx, previous.RequestID, body, false)
		}
	}
	branchRecovery := requiresIssueBranchRecovery(previous)
	var verificationPRURL string
	if s.IssueRecovery != nil && remote.Claim.ID != "" {
		claimedIssue := &config.GitHubIssueConfig{Repository: repository, Number: number, Claim: &remote.Claim}
		pr, err := s.findIssuePR(ctx, claimedIssue)
		if err != nil {
			return issueRetryResult{}, 502, err
		}
		if err := pr.recoveryBlocker(claimedIssue); err != nil {
			return issueRetryResult{}, http.StatusConflict, err
		}
		if previous.State == localruntime.IssueStateMergedUnverified {
			return issueRetryResult{}, http.StatusConflict, errors.New(mergedUnverifiedIssueError)
		}
		if previous.Version == 2 && previous.InstanceID == remote.InstanceID && previous.InstanceID == s.instanceID() && pr.mergedUnverified(previous) {
			if err := s.recordMergedUnverifiedIssuePR(previous, pr); err != nil {
				return issueRetryResult{}, http.StatusServiceUnavailable, err
			}
			return issueRetryResult{}, http.StatusConflict, errors.New(mergedUnverifiedIssueError)
		}
		if metadataRetry {
			if pr.URL == "" || !strings.EqualFold(pr.URL, previous.PRURL) ||
				pr.Closed || pr.Merged || previous.InstanceID != s.instanceID() {
				return issueRetryResult{}, http.StatusConflict, fmt.Errorf("PR metadata retry requires the original pull request and its local claim")
			}
			verificationPRURL = pr.URL
		} else if previous.Version == 2 && previous.InstanceID == s.instanceID() && !branchRecovery && pr.resumesVerification(previous) {
			verificationPRURL = pr.URL
		}
		if pr.URL != "" && verificationPRURL == "" && (automatic || branchRecovery || workflowRecovery || ownedWorkflowRetry) {
			if !automatic && !workflowRecovery && branchRecovery && previous.Version == 2 &&
				previous.InstanceID == remote.InstanceID && previous.InstanceID == s.instanceID() {
				if err := s.verifyRecoveredIssuePR(ctx, previous, pr); err != nil {
					status := http.StatusBadGateway
					if errors.Is(err, app.ErrIssueBranchRecovery) {
						status = http.StatusConflict
					}
					return issueRetryResult{}, status, err
				}
				if !pr.resumesVerification(previous) {
					if err := s.recordVerifiedIssuePR(previous, pr); err != nil {
						return issueRetryResult{}, 503, err
					}
					return issueRetryResult{PRURL: pr.URL}, http.StatusOK, nil
				}
				verificationPRURL = pr.URL
			} else {
				return issueRetryResult{}, 409, fmt.Errorf("issue already has an open or merged PR")
			}
		}
	}
	issue, err := s.fetchGitHubIssue(ctx, repository, number)
	if err != nil {
		return issueRetryResult{}, 502, err
	}
	if !strings.EqualFold(issue.State, "open") {
		return issueRetryResult{}, 409, fmt.Errorf("closed issues cannot be retried")
	}
	target, err := s.issueClaimTarget(ctx, repository, number)
	if err != nil {
		return issueRetryResult{}, 502, err
	}
	if (automatic || workflowRecovery || ownedWorkflowRetry) && target.BlockedBy > 0 {
		return issueRetryResult{}, 409, fmt.Errorf("issue is blocked by %d open issue(s)", target.BlockedBy)
	}
	closingVerificationPR := verificationPRURL != "" && strings.EqualFold(verificationPRURL, target.OnlyClosingPRURL)
	if (automatic || branchRecovery || workflowRecovery || ownedWorkflowRetry) && target.ClosingPRs > 0 && !closingVerificationPR {
		return issueRetryResult{}, 409, fmt.Errorf("%w: reconcile the issue's existing pull requests before using Recover / retry", app.ErrIssueBranchRecovery)
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
	// Automatic recovery needs the matching local intent for that incomplete
	// admission; explicit starts may also recreate a deleted branch.
	incompleteReservation := remote.Claim.ID == "" || (previous.Version == 2 &&
		previous.ID == remote.Claim.ID && previous.InstanceID == remote.InstanceID &&
		previous.InstanceID == s.instanceID() && !previous.BranchReserved)
	reserve := target.Linked == 0 && target.ClosingPRs == 0 && savedRun != nil && incompleteReservation
	if !automatic || !reserve {
		// Development links can be stale. Check the exact ref before creating
		// another attempt or offering to reuse the reserved branch.
		exists, err := s.issueBranchExists(ctx, repository, branch)
		if err != nil {
			return issueRetryResult{}, 502, err
		}
		if !exists && (automatic || branchRecovery || workflowRecovery || previous.WorkspaceDir != "") {
			return issueRetryResult{}, 409, missingReservedIssueRef(branch)
		}
		if (workflowRecovery || ((automatic || branchRecovery) && target.Linked > 0)) && (!linked || target.Linked != 1) {
			return issueRetryResult{}, 409, fmt.Errorf("%w: reconcile the issue's linked branches with %q, then use Recover / retry",
				app.ErrIssueBranchRecovery, branch)
		}
		reserve = !exists
	}
	if err := verifyIssueRetryCheckout(ctx, previous, branch); err != nil {
		return issueRetryResult{}, http.StatusConflict, err
	}
	base := remote.Claim.BaseBranch
	if base == "" && savedRun != nil {
		base = savedRun.GitHubIssue.Claim.BaseBranch
	}
	if base == "" && requestedRun != nil {
		base = requestedRun.BaseBranch
	}
	if base == "" {
		base = target.BaseBranch
	}
	run := issueRunConfig(GitHubRepo{FullName: repository, DefaultBranch: base}, issue, settings.GitHubIssueLinkMode)
	if requestedRun != nil {
		run = *requestedRun
	}
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
	if automatic {
		currentExclusion := issueExclusionLabel(settings)
		ownsCurrentLabel := previous.LabelAdded && strings.EqualFold(previous.Run.GitHubIssue.Claim.ExclusionLabel, currentExclusion)
		if !ownsCurrentLabel && githubIssueHasAnyLabel(issue, []string{currentExclusion}) {
			return issueRetryResult{}, 409, fmt.Errorf("issue was excluded after its worker stopped")
		}
		exclusion = currentExclusion
	}
	claim := &config.GitHubIssueClaim{ID: newIssueClaimID(), Branch: branch, BaseBranch: base, ExclusionLabel: exclusion, Attempt: remote.Claim.Attempt + 1}
	// Copy rather than mutating a stored config's issue pointer.
	runIssue := *run.GitHubIssue
	run.GitHubIssue = &runIssue
	runIssue.Claim = claim
	resumeReview := previous.Outcome == workflow.OutcomeReviewPending && previous.PRVerification == "" &&
		verificationPRURL != "" && run.Workflow != nil
	if metadataRetry || resumeReview {
		if verificationPRURL == "" || run.Workflow == nil {
			return issueRetryResult{}, http.StatusConflict, fmt.Errorf("PR metadata retry requires the original pull request and workflow")
		}
		policy := *run.Workflow
		policy.ResumePRURL = verificationPRURL
		run.Workflow = &policy
	}
	run.BaseBranch = base
	setGitHubIssuePrompt(&run, issue)
	if err := githubassets.Validate(run.Assets); err != nil {
		return issueRetryResult{}, 400, err
	}
	var recoverySHA string
	if workflowRecovery {
		configured := strings.TrimSpace(savedRun.BaseBranch)
		if configured == "" {
			configured = target.BaseBranch
		}
		if configured != base {
			return issueRetryResult{}, http.StatusConflict, fmt.Errorf("reserved branch base does not match the workflow base")
		}
		var status int
		recoverySHA, status, err = s.verifyObservedWorkflowBranch(ctx, repository, branch, base, target)
		if err != nil {
			return issueRetryResult{}, status, err
		}
	}
	// A stopped verifier can retry on the published draft, or before PR creation
	// if the publication response was lost. Retain the original workflow policy.
	verificationWorkflowRetry := automatic && run.Workflow != nil &&
		(previous.PRVerification != "" || previous.Outcome == workflow.OutcomeReviewPending)
	if workflowRecovery || ownedWorkflowRetry || verificationWorkflowRetry {
		var retryOf string
		if ownedWorkflowRetry || verificationWorkflowRetry {
			// A stopped task can remain in the broker after its workflow run was
			// pruned. Give its replacement a distinct, restart-stable event.
			prior := *run.Workflow
			retryOf = prior.EventID
			if retryOf == "" {
				return issueRetryResult{}, http.StatusConflict, fmt.Errorf("issue recovery requires a saved workflow event")
			}
			event := sha256.Sum256([]byte(retryOf + "/" + previous.ID))
			prior.EventID = hex.EncodeToString(event[:16])
			run.Workflow = &prior
		}
		if err := s.prepareWorkflowIssueRecovery(run, retryOf); err != nil {
			return issueRetryResult{}, http.StatusConflict, err
		}
	}
	record := localruntime.IssueClaimRecord{ID: claim.ID, InstanceID: s.instanceID(), Phase: "reserving", Run: run}
	if s.IssueRecovery != nil {
		if record.InstanceID == "" {
			return issueRetryResult{}, 503, fmt.Errorf("persistent instance identity is unavailable")
		}
		record.Version = 2
		record.State = "reserving"
		record.IssueNodeID = target.ID
		record.BranchReserved = !reserve
		record.ReservationSHA = recoverySHA
		record.RequireReservationSHA = recoverySHA != ""
		record.WorkspaceDir = previous.WorkspaceDir
		// Older records can prove checkout completion through a later stage.
		switch previous.Step {
		case "agent", "codex", "claude", "git", "workflow", "pr":
			record.CheckoutComplete = true
		}
		record.CheckoutComplete = record.CheckoutComplete || previous.CheckoutComplete || previous.PublicationReady
		record.PublicationReady = previous.PublicationReady
		record.PRVerification = previous.PRVerification
		if record.PRVerification != "" {
			record.PRURL = verificationPRURL
		}
		if (record.PRVerification != "" || resumeReview) && previous.InstanceID == record.InstanceID {
			record.RequestID = previous.RequestID
		}
		if automatic {
			record.AgentAttempts = previous.AgentAttempts
			record.PriorRunFailures = issueRunFailures(previous)
			record.Failures = previous.Failures
		}
	}
	if err = s.IssueClaims.Save(record); err != nil {
		return issueRetryResult{}, 503, err
	}
	var sha string
	if record.Version == 2 {
		ref := branch
		if reserve {
			ref = base
		}
		if record.ReservationSHA == "" {
			sha, err = s.issueBranchSHA(ctx, repository, ref)
			if err != nil {
				return issueRetryResult{}, 502, s.blockIssueClaim(record, err)
			}
			record.ReservationSHA = sha
			if err := s.IssueClaims.Save(record); err != nil {
				return issueRetryResult{}, 503, err
			}
		}
		requestID, err := s.resumeIssueAdmission(ctx, record, submit)
		if err != nil {
			return issueRetryResult{}, 409, err
		}
		if workflowRecovery || ownedWorkflowRetry || verificationWorkflowRetry {
			s.Workflows.RequestScan()
		}
		return issueRetryResult{RequestID: requestID}, http.StatusAccepted, nil
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
		return issueRetryResult{}, 409, s.blockIssueClaim(record, err)
	}
	requestID, err := s.finishIssueAdmission(ctx, record, submit)
	if err != nil {
		return issueRetryResult{}, http.StatusConflict, err
	}
	return issueRetryResult{RequestID: requestID}, http.StatusAccepted, nil
}

func hasUnsubmittedWorkflowRecovery(record localruntime.IssueClaimRecord) bool {
	return record.RequireReservationSHA && record.Phase != "submitted" && record.StartedAt.IsZero() && record.Outcome == ""
}

// Compare immutable commit IDs so a moved branch cannot change the comparison.
func (s Server) verifyObservedWorkflowBranch(ctx context.Context, repository, branch, base string, target issueClaimTarget) (string, int, error) {
	head, err := s.issueBranchSHA(ctx, repository, branch)
	if err != nil {
		return "", http.StatusBadGateway, err
	}
	baseSHA := target.SHA
	if base != target.BaseBranch {
		baseSHA, err = s.issueBranchSHA(ctx, repository, base)
		if err != nil {
			return "", http.StatusBadGateway, err
		}
	}
	if head == baseSHA {
		return head, 0, nil
	}
	result, err := s.runIssueGH(ctx, execx.Command{Name: "gh", Args: []string{"api", "repos/" + repository + "/compare/" + baseSHA + "..." + head}})
	if err != nil {
		return "", http.StatusBadGateway, err
	}
	var comparison struct {
		Ahead *int `json:"ahead_by"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &comparison); err != nil {
		return "", http.StatusBadGateway, err
	}
	if comparison.Ahead == nil || *comparison.Ahead != 0 {
		return "", http.StatusConflict, fmt.Errorf("inspect and reconcile commits on %q before recovering through the workflow", branch)
	}
	return head, 0, nil
}

func issueRepositoryMatchesRun(value, repository string) bool {
	return githubutil.RepositoryMatches(value, repository)
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
