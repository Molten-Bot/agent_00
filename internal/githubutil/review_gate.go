package githubutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const ReviewCheckName = "agent00 / review-policy"
const CleanReviewVerdict = `<!-- agent00-review-verdict {"status":"clean","mergeReady":true,"findings":[]} -->`

var (
	ErrReviewCheckPending     = errors.New("waiting for the trusted review policy check on the current pull request head")
	ErrReviewInterrupted      = errors.New("pull request merged externally before the review gate completed")
	ErrReviewThreadsPending   = errors.New("waiting for unresolved review threads")
	ErrReviewChangesRequested = errors.New("waiting for requested changes")
	ErrReviewApprovalPending  = errors.New("waiting for human approval on the current pull request head")
)

// ReviewPolicy records the policy selected when the review was run.
type ReviewPolicy struct {
	FixPasses  int    `json:"fixPasses"`
	Complexity string `json:"complexity,omitempty"`
	Hash       string `json:"hash,omitempty"`
	CheckAppID int64  `json:"checkAppId,omitempty"`
}

// ReviewReceipt binds one structured result to a submitted GitHub review.
type ReviewReceipt struct {
	PRURL        string          `json:"prUrl"`
	Head         string          `json:"head"`
	Run          string          `json:"run"`
	Pass         int             `json:"pass"`
	Policy       ReviewPolicy    `json:"policy"`
	Result       json.RawMessage `json:"result"`
	Marker       string          `json:"marker"`
	ReviewID     int64           `json:"reviewId"`
	ReviewNodeID string          `json:"reviewNodeId,omitempty"`
	URL          string          `json:"url"`
}

func (r ReviewReceipt) Clean() bool {
	var verdict struct {
		Status     string
		MergeReady bool
		Findings   []json.RawMessage
	}
	return r.ReviewID > 0 && r.Head != "" && r.Run != "" && r.Pass > 0 &&
		r.Marker != "" && r.URL != "" && PullRequestRepository(r.PRURL) != "" &&
		json.Unmarshal(r.Result, &verdict) == nil && verdict.Status == "clean" &&
		verdict.MergeReady && len(verdict.Findings) == 0
}

type ReviewCommand func(context.Context, ...string) (string, error)

type SubmittedReview struct {
	ID          int64  `json:"id"`
	NodeID      string `json:"node_id"`
	Head        string `json:"commit_id"`
	URL         string `json:"html_url"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
	Association string `json:"author_association"`
	User        struct{ Login, Type string }
}

func (r SubmittedReview) Matches(head, marker string) bool {
	return r.ID > 0 && r.URL != "" && r.Head == head && r.State == "COMMENTED" &&
		r.SubmittedAt != "" && strings.HasPrefix(r.Body, marker+"\n")
}

func ReviewEndpoint(prURL string) string {
	return "repos/" + PullRequestRepository(prURL) + "/pulls/" + PullRequestSelector(prURL) + "/reviews"
}

// ListSubmittedReviews reads every REST page. gh propagates HTTP errors from
// later pages. Empty, null, or truncated responses are invalid.
func ListSubmittedReviews(ctx context.Context, gh ReviewCommand, prURL string) ([]SubmittedReview, error) {
	raw, err := gh(ctx, "api", "--paginate", ReviewEndpoint(prURL)+"?per_page=100")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	reviews := []SubmittedReview{}
	readPage := false
	for {
		var page []SubmittedReview
		err := dec.Decode(&page)
		if err == io.EOF && readPage {
			return reviews, nil
		}
		if err != nil || page == nil {
			return nil, fmt.Errorf("incomplete pull request review list")
		}
		readPage = true
		reviews = append(reviews, page...)
	}
}

// VerifyReviewGate checks the live PR, the persisted receipt and all threads.
// Callers must still use a SHA precondition when merging.
func VerifyReviewGate(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt) error {
	return verifyReviewGate(ctx, gh, receipt, false, false)
}

// VerifyReviewMergeGate also checks the workflow's human approval requirement
// and protects CLI merges from switching to a deferred merge queue.
func VerifyReviewMergeGate(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt, requireApproval, directCLI bool) error {
	return verifyReviewGate(ctx, gh, receipt, requireApproval, directCLI)
}

func verifyReviewGate(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt, requireApproval, directCLI bool) error {
	if !receipt.Clean() {
		return fmt.Errorf("waiting for a submitted clean review receipt")
	}
	repo := PullRequestRepository(receipt.PRURL)
	raw, err := gh(ctx, "pr", "view", PullRequestSelector(receipt.PRURL), "--repo", repo, "--json", "state,headRefOid,reviewDecision")
	if err != nil {
		return err
	}
	var pr struct {
		State, HeadRefOID string
		ReviewDecision    json.RawMessage
	}
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return fmt.Errorf("decode pull request gate state: %w", err)
	}
	if pr.State == "MERGED" {
		return ErrReviewInterrupted
	}
	if pr.State != "OPEN" || pr.HeadRefOID != receipt.Head {
		return fmt.Errorf("pull request is closed or head changed since review")
	}
	var decision string
	if len(pr.ReviewDecision) == 0 || json.Unmarshal(pr.ReviewDecision, &decision) != nil {
		return fmt.Errorf("pull request review decision is unavailable")
	}
	switch decision {
	case "", "APPROVED", "REVIEW_REQUIRED":
	case "CHANGES_REQUESTED":
		return ErrReviewChangesRequested
	default:
		return fmt.Errorf("unknown pull request review decision %q", decision)
	}

	reviews, err := ListSubmittedReviews(ctx, gh, receipt.PRURL)
	if err != nil {
		return err
	}
	found, seenReceipt := false, false
	latest := map[string]SubmittedReview{}
	for _, review := range reviews {
		if review.ID == receipt.ReviewID {
			if seenReceipt {
				return fmt.Errorf("duplicate submitted review in pull request review list")
			}
			seenReceipt = true
			found = review.Matches(receipt.Head, receipt.Marker) &&
				strings.HasPrefix(review.Body, receipt.Marker+"\n"+CleanReviewVerdict+"\n") &&
				strings.Contains(review.Body, "No material issues found.")
		}
		switch review.State {
		case "CHANGES_REQUESTED", "APPROVED", "DISMISSED":
			if review.User.Login == "" {
				return fmt.Errorf("cannot identify author of submitted review")
			}
			latest[strings.ToLower(review.User.Login)] = review
		}
	}
	if !found {
		return fmt.Errorf("submitted clean review is missing, dismissed, or stale")
	}
	humanApproved := false
	for _, review := range latest {
		if review.State == "CHANGES_REQUESTED" {
			return ErrReviewChangesRequested
		}
		if review.State == "APPROVED" && review.Head == receipt.Head && review.User.Type == "User" {
			switch review.Association {
			case "OWNER", "MEMBER", "COLLABORATOR":
				humanApproved = true
			}
		}
	}
	if requireApproval && !humanApproved {
		return ErrReviewApprovalPending
	}
	threads, err := reviewThreads(ctx, gh, receipt.PRURL, receipt.Head, directCLI)
	if err != nil {
		return err
	}
	for _, thread := range threads {
		if !*thread.Resolved {
			return ErrReviewThreadsPending
		}
	}
	return nil
}

type ReviewThread struct {
	ID       string `json:"id"`
	Resolved *bool  `json:"isResolved"`
	Comments struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Body   string
			Review struct {
				ID string `json:"id"`
			} `json:"pullRequestReview"`
		}
	}
}

func ReviewThreads(ctx context.Context, gh ReviewCommand, prURL, head string) ([]ReviewThread, error) {
	return reviewThreads(ctx, gh, prURL, head, false)
}

func reviewThreads(ctx context.Context, gh ReviewCommand, prURL, head string, directCLI bool) ([]ReviewThread, error) {
	const query = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String) {
		repository(owner:$owner,name:$repo) { pullRequest(number:$number) {
			state headRefOid isMergeQueueEnabled reviewThreads(first:100,after:$cursor) {
				pageInfo { hasNextPage endCursor }
				nodes { id isResolved comments(first:1) { totalCount nodes { body pullRequestReview { id } } } }
			}
		} }
	}`
	repo := strings.Split(PullRequestRepository(prURL), "/")
	if len(repo) != 2 || head == "" {
		return nil, fmt.Errorf("missing review thread target")
	}
	base := []string{"api", "graphql", "-f", "query=" + query, "-f", "owner=" + repo[0], "-f", "repo=" + repo[1], "-F", "number=" + PullRequestSelector(prURL)}
	var threads []ReviewThread
	seenCursors := map[string]bool{}
	seenThreads := map[string]bool{}
	cursor := ""
	for {
		args := append([]string(nil), base...)
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		}
		raw, err := gh(ctx, args...)
		if err != nil {
			return nil, err
		}
		var page struct {
			Errors []json.RawMessage
			Data   struct {
				Repository struct {
					PullRequest struct {
						State, HeadRefOID   string
						IsMergeQueueEnabled *bool
						ReviewThreads       struct {
							PageInfo struct {
								HasNextPage *bool
								EndCursor   string
							}
							Nodes []ReviewThread
						}
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Errors) != 0 {
			return nil, fmt.Errorf("cannot read complete pull request review threads")
		}
		pr := page.Data.Repository.PullRequest
		if pr.State == "MERGED" {
			return nil, ErrReviewInterrupted
		}
		if pr.State != "OPEN" || pr.HeadRefOID != head {
			return nil, fmt.Errorf("pull request is closed or head changed while reading review threads")
		}
		if directCLI && (pr.IsMergeQueueEnabled == nil || *pr.IsMergeQueueEnabled) {
			return nil, fmt.Errorf("cannot verify a direct merge without a merge queue")
		}
		list := pr.ReviewThreads
		if list.Nodes == nil || list.PageInfo.HasNextPage == nil {
			return nil, fmt.Errorf("incomplete pull request review thread page")
		}
		for _, thread := range list.Nodes {
			if thread.ID == "" || thread.Resolved == nil || seenThreads[thread.ID] {
				return nil, fmt.Errorf("incomplete pull request review thread")
			}
			seenThreads[thread.ID] = true
		}
		threads = append(threads, list.Nodes...)
		if !*list.PageInfo.HasNextPage {
			return threads, nil
		}
		cursor = list.PageInfo.EndCursor
		if cursor == "" || seenCursors[cursor] {
			return nil, fmt.Errorf("incomplete pull request review thread pagination")
		}
		seenCursors[cursor] = true
	}
}

func ResolveReviewThread(ctx context.Context, gh ReviewCommand, id string) error {
	const query = `mutation($id:ID!) { resolveReviewThread(input:{threadId:$id}) { thread { id isResolved } } }`
	raw, err := gh(ctx, "api", "graphql", "-f", "query="+query, "-f", "id="+id)
	if err != nil {
		return err
	}
	var result struct {
		Errors []json.RawMessage
		Data   struct {
			ResolveReviewThread struct {
				Thread struct {
					ID         string
					IsResolved bool
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Errors) != 0 ||
		result.Data.ResolveReviewThread.Thread.ID != id || !result.Data.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("GitHub did not confirm review thread resolution")
	}
	return nil
}

func (r ReviewReceipt) EvidenceURL() string {
	if r.ReviewID <= 0 {
		return ""
	}
	return r.PRURL + "#pullrequestreview-" + strconv.FormatInt(r.ReviewID, 10)
}

// RequiredReviewCheckApp reads the opt-in for the target repository.
func RequiredReviewCheckApp(repo string) (int64, error) {
	for _, target := range strings.Fields(os.Getenv("HARNESS_REVIEW_CHECK_REPOSITORIES")) {
		if strings.EqualFold(target, repo) {
			appID, err := strconv.ParseInt(os.Getenv("HARNESS_REVIEW_CHECK_APP_ID"), 10, 64)
			if err != nil || appID <= 0 {
				return 0, fmt.Errorf("review policy enforcement requires a configured GitHub App ID")
			}
			return appID, nil
		}
	}
	return 0, nil
}

// ReviewCheckApp preserves the captured enforcement policy across restarts.
func (r ReviewReceipt) ReviewCheckApp() (int64, error) {
	appID, err := RequiredReviewCheckApp(PullRequestRepository(r.PRURL))
	if err != nil || r.Policy.CheckAppID == 0 {
		return appID, err
	}
	if appID != 0 && appID != r.Policy.CheckAppID {
		return 0, fmt.Errorf("trusted review policy app changed since review")
	}
	return r.Policy.CheckAppID, nil
}

// VerifyReviewCheckRules verifies that the target branch actually requires
// the check from the configured integration.
func VerifyReviewCheckRules(ctx context.Context, gh ReviewCommand, appID int64, receipt ReviewReceipt) error {
	repo, number := PullRequestRepository(receipt.PRURL), PullRequestSelector(receipt.PRURL)
	raw, err := gh(ctx, "pr", "view", number, "--repo", repo, "--json", "state,headRefOid,baseRefName")
	if err != nil {
		return err
	}
	var pr struct{ State, HeadRefOID, BaseRefName string }
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return fmt.Errorf("decode review policy target: %w", err)
	}
	if pr.State == "MERGED" {
		return ErrReviewInterrupted
	}
	if pr.State != "OPEN" || pr.HeadRefOID != receipt.Head || pr.BaseRefName == "" {
		return fmt.Errorf("pull request changed before review policy verification")
	}
	raw, err = gh(ctx, "api", "--paginate", "repos/"+repo+"/rules/branches/"+url.PathEscape(pr.BaseRefName)+"?per_page=100")
	if err != nil {
		return err
	}
	return verifyRequiredReviewCheck(raw, appID)
}

type reviewCheck struct {
	ID                       int64
	Head                     string `json:"head_sha"`
	ExternalID               string `json:"external_id"`
	Name, Status, Conclusion string
	App                      struct{ ID int64 }
}

func checkExternalID(receipt ReviewReceipt) string {
	return fmt.Sprintf("pr-%s-review-%d", PullRequestSelector(receipt.PRURL), receipt.ReviewID)
}

func (c reviewCheck) matches(appID int64, receipt ReviewReceipt) bool {
	return c.ID > 0 && c.Head == receipt.Head && c.ExternalID == checkExternalID(receipt) &&
		c.App.ID == appID && c.Name == ReviewCheckName && c.Status == "completed" && c.Conclusion == "success"
}

func HasPublishedReviewCheck(ctx context.Context, gh ReviewCommand, appID int64, receipt ReviewReceipt) (bool, error) {
	endpoint := "repos/" + PullRequestRepository(receipt.PRURL) + "/commits/" + url.PathEscape(receipt.Head) + "/check-runs"
	query := url.Values{"app_id": {strconv.FormatInt(appID, 10)}, "check_name": {ReviewCheckName}, "per_page": {"100"}}
	raw, err := gh(ctx, "api", "--paginate", endpoint+"?"+query.Encode())
	if err != nil {
		return false, err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	found, readPage := false, false
	for {
		var page struct {
			Checks []reviewCheck `json:"check_runs"`
		}
		if err := dec.Decode(&page); err == io.EOF && readPage {
			return found, nil
		} else if err != nil || page.Checks == nil {
			return false, fmt.Errorf("cannot read complete review policy checks")
		}
		readPage = true
		for _, check := range page.Checks {
			found = found || check.matches(appID, receipt)
		}
	}
}

func VerifyPublishedReviewCheck(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt) error {
	appID, err := receipt.ReviewCheckApp()
	if err != nil || appID == 0 {
		return err
	}
	if err := VerifyReviewCheckRules(ctx, gh, appID, receipt); err != nil {
		return err
	}
	published, err := HasPublishedReviewCheck(ctx, gh, appID, receipt)
	if err != nil {
		return err
	}
	if !published {
		return ErrReviewCheckPending
	}
	return nil
}

func PublishReviewCheck(ctx context.Context, gh ReviewCommand, appID int64, receipt ReviewReceipt) error {
	if err := VerifyReviewCheckRules(ctx, gh, appID, receipt); err != nil {
		return err
	}
	if err := VerifyReviewGate(ctx, gh, receipt); err != nil {
		return err
	}
	found, err := HasPublishedReviewCheck(ctx, gh, appID, receipt)
	if err != nil || found {
		return err
	}
	endpoint := "repos/" + PullRequestRepository(receipt.PRURL) + "/check-runs"
	raw, err := gh(ctx, "api", "--method", "POST", endpoint,
		"-f", "name="+ReviewCheckName, "-f", "head_sha="+receipt.Head,
		"-f", "status=completed", "-f", "conclusion=success",
		"-f", "external_id="+checkExternalID(receipt), "-f", "details_url="+receipt.EvidenceURL(),
		"-f", "output[title]=Submitted clean review",
		"-f", "output[summary]=No material issues found.")
	if err != nil {
		if found, lookupErr := HasPublishedReviewCheck(ctx, gh, appID, receipt); lookupErr == nil && found {
			return nil
		}
		return fmt.Errorf("publish required review policy check: %w", err)
	}
	var check reviewCheck
	if err := json.Unmarshal([]byte(raw), &check); err != nil || !check.matches(appID, receipt) {
		return fmt.Errorf("GitHub did not acknowledge the required check from the configured app")
	}
	return nil
}

func verifyRequiredReviewCheck(raw string, appID int64) error {
	type rule struct {
		Type       string
		Parameters struct {
			Checks []struct {
				Context       string
				IntegrationID int64 `json:"integration_id"`
			} `json:"required_status_checks"`
			ThreadResolution bool `json:"required_review_thread_resolution"`
		}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	required, threads := false, false
	for {
		var page []rule
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil || page == nil {
			return fmt.Errorf("cannot verify active target branch rules")
		}
		for _, rule := range page {
			switch rule.Type {
			case "merge_queue":
				return fmt.Errorf("review policy enforcement requires a target branch without a merge queue")
			case "pull_request":
				threads = threads || rule.Parameters.ThreadResolution
			case "required_status_checks":
				for _, check := range rule.Parameters.Checks {
					if check.Context == ReviewCheckName {
						if check.IntegrationID != appID || appID <= 0 {
							return fmt.Errorf("active rules do not pin %q to the configured GitHub App", ReviewCheckName)
						}
						required = true
					}
				}
			}
		}
	}
	if !required || !threads {
		return fmt.Errorf("target branch must require %q from the configured GitHub App and require resolved review conversations", ReviewCheckName)
	}
	return nil
}

// MergeReviewedHead requests an immediate merge with a GitHub-side SHA
// precondition. It cannot arm a deferred merge or bypass repository rules.
func MergeReviewedHead(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt, method string) error {
	if !receipt.Clean() {
		return fmt.Errorf("waiting for a submitted clean review receipt")
	}
	endpoint := "repos/" + PullRequestRepository(receipt.PRURL) + "/pulls/" + PullRequestSelector(receipt.PRURL) + "/merge"
	raw, err := gh(ctx, "api", "--method", "PUT", endpoint, "-f", "sha="+receipt.Head, "-f", "merge_method="+method)
	if err != nil {
		return fmt.Errorf("merge reviewed pull request: %w", err)
	}
	var result struct {
		Merged  bool
		Message string
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return fmt.Errorf("decode GitHub merge acknowledgement: %w", err)
	}
	if !result.Merged {
		return fmt.Errorf("GitHub declined the reviewed merge: %s", result.Message)
	}
	return nil
}
