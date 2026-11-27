package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

type GitHub struct {
	Runner   execx.Runner
	Cooldown *githubutil.Cooldown
	Logf     func(string, ...any)
}

var errAppMergeVerificationPending = errors.New("could not verify the app merge")

// pullRequestSnapshotError preserves the complete issue snapshot when a pull
// request listed by GitHub cannot be inspected.
type pullRequestSnapshotError struct{ err error }

func (e *pullRequestSnapshotError) Error() string { return e.err.Error() }
func (e *pullRequestSnapshotError) Unwrap() error { return e.err }

func (g GitHub) command(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	r, err := g.Cooldown.Run(ctx, g.Runner, execx.Command{Name: "gh", Args: args}, g.Cooldown != nil)
	if err != nil {
		return "", fmt.Errorf("GitHub %s: %w", args[0], err)
	}
	return r.Stdout, nil
}

// ScanNeeds combines the GitHub data required by enabled triggers in one repository.
type ScanNeeds struct {
	Issues, PullRequests, Viewer, LinkedIssues, Reviews, Checks bool

	// A selected Actions workflow is fetched independently of issue/PR scans.
	ActionWorkflow string
}

func (n *ScanNeeds) include(w Definition) {
	if w.Trigger.Event == ActionFailedEvent {
		n.ActionWorkflow = strings.TrimSpace(w.Trigger.Value)
		return
	}
	n.Issues = n.Issues || strings.HasPrefix(w.Trigger.Event, "issue.")
	n.PullRequests = n.PullRequests || strings.HasPrefix(w.Trigger.Event, "pr.")
	n.Reviews = n.Reviews || w.Trigger.Event == "pr.changes_requested"
	n.Checks = n.Checks || w.Trigger.Event == "pr.checks_failed"
	n.Viewer = n.Viewer || (w.Trigger.Value == "@me" && contains([]string{"issue.assigned", "pr.assigned", "pr.review_requested"}, w.Trigger.Event))
	for _, rule := range w.Filters.Rules {
		n.LinkedIssues = n.LinkedIssues || rule.Field == "linkedIssue"
		n.Viewer = n.Viewer || (rule.Value == "@me" && contains([]string{"author", "assignee", "reviewer"}, rule.Field))
	}
}

// Each scan owns its counter. Pagination can make several HTTP requests in one gh invocation.
type scanRunner struct {
	execx.Runner
	calls int
}

func (r *scanRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	r.calls++
	return r.Runner.Run(ctx, cmd)
}

func (g GitHub) Fetch(ctx context.Context, repo string, needs ScanNeeds) ([]Item, error) {
	ctx = githubutil.WithDefaultCaller(ctx, "workflow_discovery")
	if g.Logf != nil {
		start := time.Now()
		runner := &scanRunner{Runner: g.Runner}
		g.Runner = runner
		defer func() {
			g.Logf("workflow event=scan gh_calls=%d duration_ms=%d", runner.calls, time.Since(start).Milliseconds())
		}()
	}
	if !repoPattern.MatchString(repo) {
		return nil, fmt.Errorf("invalid repository")
	}
	if err := g.Cooldown.CheckDiscovery(); err != nil {
		return nil, err
	}
	if needs.ActionWorkflow != "" {
		return g.fetchActionRuns(ctx, repo, needs.ActionWorkflow)
	}
	var viewer string
	if needs.Viewer {
		raw, err := g.command(ctx, "api", "user", "--jq", ".login", "--cache", "10m")
		if err != nil {
			return nil, err
		}
		viewer = strings.TrimSpace(raw)
	}
	resource := "issues"
	if !needs.Issues {
		resource = "pulls"
	}
	return g.fetchCollection(ctx, repo, resource, viewer, needs)
}

func (g GitHub) fetchActionRuns(ctx context.Context, repo, selector string) ([]Item, error) {
	if !actionWorkflowPattern.MatchString(selector) {
		return nil, fmt.Errorf("invalid GitHub Actions workflow selector")
	}
	// Bound polling to the most recent runs of the selected workflow. Include
	// in-progress runs so the same window is used regardless of conclusion.
	raw, err := g.command(ctx, "api", "repos/"+repo+"/actions/workflows/"+selector+"/runs?per_page=100")
	if err != nil {
		return nil, err
	}
	var response struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Attempt    int    `json:"run_attempt"`
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
			Name       string
			Status     string
			Conclusion string
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("decode GitHub Actions runs: %w", err)
	}
	if response.Runs == nil {
		return nil, fmt.Errorf("GitHub Actions response is missing workflow runs")
	}
	items := make([]Item, 0, len(response.Runs))
	for _, run := range response.Runs {
		if run.ID <= 0 || run.Attempt <= 0 {
			return nil, fmt.Errorf("GitHub Actions response is missing a run ID or attempt number")
		}
		items = append(items, Item{
			Repository: repo, Kind: "action", Title: run.Name,
			URL:         fmt.Sprintf("https://github.com/%s/actions/runs/%d/attempts/%d", repo, run.ID, run.Attempt),
			ActionRunID: run.ID, ActionRunAttempt: run.Attempt,
			HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA,
			ChecksFailed: run.Status == "completed" && run.Conclusion == "failure",
		})
	}
	return items, nil
}

func (g GitHub) fetchCollection(ctx context.Context, repo, resource, viewer string, needs ScanNeeds) ([]Item, error) {
	if err := g.Cooldown.CheckDiscovery(); err != nil {
		return nil, err
	}
	args := []string{"api", "--paginate", "repos/" + repo + "/" + resource + "?state=open&per_page=100"}
	if resource == "issues" {
		args = append(args, "-H", "X-GitHub-Api-Version: 2026-03-10")
	}
	raw, err := g.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	type issue struct {
		Number             int
		Title, Body        string
		HTMLURL            string `json:"html_url"`
		User               struct{ Login, Type string }
		Labels             []struct{ Name string }
		Assignees          []struct{ Login string }
		Type               *struct{ Name string }
		Status             json.RawMessage
		PullRequest        *json.RawMessage `json:"pull_request"`
		Draft              bool
		Head, Base         struct{ Ref, SHA string }
		RequestedReviewers []struct{ Login string }      `json:"requested_reviewers"`
		RequestedTeams     []struct{ Name, Slug string } `json:"requested_teams"`
	}
	pages, err := decodeGitHubPages[issue](raw)
	if err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	if err := g.Cooldown.CheckDiscovery(); err != nil {
		return nil, err
	}
	items := []Item{}
	var pullRequestErr error
	batchPRs := false
	basicPRs := !needs.Reviews && !needs.Checks && !needs.LinkedIssues
	for _, page := range pages {
		for _, v := range page {
			isPR := v.PullRequest != nil || resource == "pulls"
			// The pulls collection already contains the head, labels, assignees,
			// and requested reviewers required by basic PR triggers.
			listedPR := resource == "pulls" && v.Head.Ref != "" && v.Head.SHA != "" && basicPRs
			if resource == "issues" && isPR && needs.PullRequests && basicPRs {
				batchPRs = true
				continue
			}
			if isPR && !listedPR {
				if !needs.PullRequests || pullRequestErr != nil {
					continue
				}
				if err := g.Cooldown.CheckDiscovery(); err != nil {
					pullRequestErr = err
					continue
				}
				i, err := g.inspect(ctx, repo, strconv.Itoa(v.Number), needs)
				if err != nil {
					pullRequestErr = fmt.Errorf("inspect %s#%d: %w", repo, v.Number, err)
					continue
				}
				i.Viewer = viewer
				items = append(items, i)
				continue
			}
			i := Item{Repository: repo, Number: v.Number, Kind: "issue", Title: v.Title, Body: v.Body, URL: v.HTMLURL, Author: v.User.Login, Bot: v.User.Type == "Bot", Open: true}
			for _, l := range v.Labels {
				i.Labels = append(i.Labels, l.Name)
			}
			for _, a := range v.Assignees {
				i.Assignees = append(i.Assignees, a.Login)
			}
			if listedPR {
				i.Kind = "pr"
				i.Draft = v.Draft
				i.HeadBranch, i.HeadSHA, i.BaseBranch = v.Head.Ref, v.Head.SHA, v.Base.Ref
				for _, reviewer := range v.RequestedReviewers {
					i.Reviewers = append(i.Reviewers, reviewer.Login)
				}
				for _, team := range v.RequestedTeams {
					if team.Slug != "" {
						i.Reviewers = append(i.Reviewers, team.Slug)
					}
					if team.Name != "" {
						i.Reviewers = append(i.Reviewers, team.Name)
					}
				}
			}
			if !isPR && len(v.Status) != 0 {
				var status struct{ Name string }
				if err := json.Unmarshal(v.Status, &status); err != nil {
					return nil, fmt.Errorf("decode GitHub issue status: %w", err)
				}
				i.HasIssueStatus, i.IssueStatus = true, status.Name
			}
			if v.Type != nil {
				i.IssueType = v.Type.Name
			}
			i.Viewer = viewer
			items = append(items, i)
		}
	}
	if batchPRs {
		prs, err := g.fetchCollection(ctx, repo, "pulls", viewer, needs)
		if err != nil {
			return items, &pullRequestSnapshotError{err: err}
		}
		items = append(items, prs...)
	}
	if pullRequestErr != nil {
		items = slices.DeleteFunc(items, func(i Item) bool { return i.Kind == "pr" })
		return items, &pullRequestSnapshotError{err: pullRequestErr}
	}
	return items, nil
}
func (g GitHub) Inspect(ctx context.Context, repo, selector string) (Item, error) {
	return g.inspect(githubutil.WithCaller(ctx, "workflow_inspect"), repo, selector, ScanNeeds{LinkedIssues: true, Reviews: true, Checks: true})
}

func (g GitHub) InspectState(ctx context.Context, repo, selector string) (Item, error) {
	if !repoPattern.MatchString(repo) {
		return Item{}, fmt.Errorf("invalid repository")
	}
	ctx = githubutil.WithCaller(ctx, "workflow_inspect")
	owner, name, _ := strings.Cut(repo, "/")
	const query = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt autoMergeRequest{enabledAt} mergeQueueEntry{id}}}}`
	raw, err := g.command(ctx, "api", "graphql", "-f", "query="+query, "-f", "owner="+owner, "-f", "name="+name,
		"-F", "number="+githubutil.PullRequestSelector(selector), "--jq", ".data.repository.pullRequest")
	if err != nil {
		return Item{}, err
	}
	var pr struct {
		State            string
		MergedAt         time.Time
		AutoMergeRequest json.RawMessage
		MergeQueueEntry  json.RawMessage
	}
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return Item{}, fmt.Errorf("decode pull request state: %w", err)
	}
	switch pr.State {
	case "OPEN":
		if len(pr.AutoMergeRequest) == 0 || len(pr.MergeQueueEntry) == 0 {
			return Item{}, fmt.Errorf("pull request state response is missing merge request fields")
		}
		return Item{Open: true, MergeRequestInactive: string(pr.AutoMergeRequest) == "null" && string(pr.MergeQueueEntry) == "null"}, nil
	case "MERGED", "CLOSED":
		return Item{Merged: pr.State == "MERGED", MergedAt: pr.MergedAt}, nil
	default:
		return Item{}, fmt.Errorf("unexpected pull request state %q", pr.State)
	}
}

func (g GitHub) inspect(ctx context.Context, repo, selector string, needs ScanNeeds) (Item, error) {
	if !repoPattern.MatchString(repo) {
		return Item{}, fmt.Errorf("invalid repository")
	}
	fields := "number,title,body,url,author,labels,assignees,reviewRequests,baseRefName,headRefName,headRefOid,isDraft,state,mergedAt"
	if needs.Reviews {
		fields += ",reviewDecision"
	}
	if needs.Checks {
		fields += ",statusCheckRollup"
	}
	raw, err := g.command(ctx, "pr", "view", selector, "--repo", repo, "--json", fields)
	if err != nil {
		return Item{}, err
	}
	var p struct {
		Number                                                                        int
		Title, Body, URL, BaseRefName, HeadRefName, HeadRefOid, ReviewDecision, State string
		MergedAt                                                                      time.Time
		IsDraft                                                                       bool
		Author                                                                        struct {
			Login string
			IsBot bool
		}
		Labels            []struct{ Name string }
		Assignees         []struct{ Login string }
		ReviewRequests    []struct{ Login, Name, Slug string }
		StatusCheckRollup []githubutil.StatusCheck
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return Item{}, err
	}
	if p.Number < 1 {
		return Item{}, fmt.Errorf("pull request response is missing its number")
	}
	if p.State == "MERGED" || p.State == "CLOSED" {
		return Item{Repository: repo, Number: p.Number, Kind: "pr", URL: p.URL, Title: p.Title, HeadSHA: p.HeadRefOid, Merged: p.State == "MERGED", MergedAt: p.MergedAt}, nil
	}
	// Older gh versions do not expose closingIssuesReferences in pr view's
	// JSON field list. Ask GraphQL directly for the relationship count.
	linkedCount := 0
	if needs.LinkedIssues {
		parts := strings.SplitN(repo, "/", 2)
		linked, err := g.command(ctx, "api", "graphql", "-f", "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){closingIssuesReferences{totalCount}}}}", "-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "number="+strconv.Itoa(p.Number), "--jq", ".data.repository.pullRequest.closingIssuesReferences.totalCount")
		if err != nil {
			return Item{}, err
		}
		linkedCount, err = strconv.Atoi(strings.TrimSpace(linked))
		if err != nil || linkedCount < 0 {
			return Item{}, fmt.Errorf("decode linked issue count")
		}
	}
	i := Item{Repository: repo, Number: p.Number, Kind: "pr", Title: p.Title, Body: p.Body, URL: p.URL, Author: p.Author.Login, Bot: p.Author.IsBot, BaseBranch: p.BaseRefName, HeadBranch: p.HeadRefName, HeadSHA: p.HeadRefOid, Draft: p.IsDraft, LinkedIssue: linkedCount > 0, ReviewDecision: p.ReviewDecision, Open: p.State == "OPEN"}
	for _, l := range p.Labels {
		i.Labels = append(i.Labels, l.Name)
	}
	for _, a := range p.Assignees {
		i.Assignees = append(i.Assignees, a.Login)
	}
	for _, r := range p.ReviewRequests {
		if r.Login != "" {
			i.Reviewers = append(i.Reviewers, r.Login)
		} else {
			i.Reviewers = append(i.Reviewers, r.Slug)
		}
	}
	i.ChecksPassed, i.ChecksFailed = githubutil.CIStatus(p.StatusCheckRollup)
	if !needs.Reviews {
		return i, nil
	}
	reviews, err := g.command(ctx, "api", "--paginate", "repos/"+repo+"/pulls/"+strconv.Itoa(i.Number)+"/reviews?per_page=100")
	if err != nil {
		return Item{}, err
	}
	if err := applyReviews(&i, reviews); err != nil {
		return Item{}, err
	}
	return i, nil
}

var errMergePending = errors.New("GitHub did not confirm the pull request merged")

func (g GitHub) Merge(ctx context.Context, r Run) error {
	ctx = githubutil.WithCaller(ctx, "workflow_merge")
	if r.MergeHead == "" {
		return fmt.Errorf("missing reviewed head")
	}
	appID, err := githubutil.RequiredReviewCheckApp(r.Item.Repository)
	if err != nil {
		return err
	}
	if r.requiresReview() || appID > 0 {
		receipt := r.cleanReviewReceipt(r.Outcome)
		if receipt == nil || receipt.Head != r.MergeHead {
			return fmt.Errorf("waiting for a submitted clean review on the current pull request head")
		}
		if err := githubutil.VerifyPublishedReviewCheck(ctx, g.command, *receipt); err != nil {
			return err
		}
		if err := githubutil.VerifyReviewMergeGate(ctx, g.command, *receipt, r.Workflow.Merge.RequireApproval, r.Workflow.Merge.DeleteBranch); err != nil {
			return err
		}
		if !r.Workflow.Merge.DeleteBranch {
			return githubutil.MergeReviewedHead(ctx, g.command, *receipt, r.Workflow.Merge.Method)
		}
	} else if err := githubutil.VerifyReviewThreads(ctx, g.command, r.PRURL, r.MergeHead); err != nil {
		return err
	}
	args := []string{"pr", "merge", r.PRURL, "--repo", r.Item.Repository, "--" + r.Workflow.Merge.Method, "--match-head-commit", r.MergeHead}
	if r.Workflow.Merge.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	if _, err = g.command(ctx, args...); err != nil {
		return err
	}
	raw, err := g.command(ctx, "pr", "view", r.PRURL, "--repo", r.Item.Repository, "--json", "state")
	if err != nil {
		return errors.Join(errMergePending, errAppMergeVerificationPending, err)
	}
	var pr struct{ State string }
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return errors.Join(errMergePending, errAppMergeVerificationPending, err)
	}
	switch pr.State {
	case "MERGED":
		return nil
	case "OPEN", "CLOSED":
		return errMergePending
	default:
		return errors.Join(errMergePending, errAppMergeVerificationPending)
	}
}

// CanonicalRepository accepts the forms used by the harness's run config.
func CanonicalRepository(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "ssh://git@github.com/")
	value = strings.TrimPrefix(value, "ssh://github.com/")
	value = strings.TrimPrefix(value, "github.com/")
	value = strings.TrimPrefix(value, "https://github.com/")
	value = strings.TrimPrefix(value, "git@github.com:")
	return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git"))
}

// GitHub reviewDecision may be empty on unprotected repositories. Inspect actual
// submitted reviews as well; an outsider or bot cannot satisfy human approval.
func applyReviews(i *Item, raw string) error {
	type review struct {
		State       string
		CommitID    string `json:"commit_id"`
		Association string `json:"author_association"`
		User        struct{ Login, Type string }
	}
	pages, err := decodeGitHubPages[review](raw)
	if err != nil {
		return fmt.Errorf("decode pull request reviews: %w", err)
	}
	latest := map[string]review{}
	for _, page := range pages {
		for _, r := range page {
			if r.User.Login != "" && contains([]string{"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}, r.State) {
				latest[strings.ToLower(r.User.Login)] = r
			}
		}
	}
	for _, r := range latest {
		if r.State == "CHANGES_REQUESTED" {
			i.ReviewDecision = "CHANGES_REQUESTED"
		}
		if r.State == "APPROVED" && r.CommitID == i.HeadSHA && r.User.Type == "User" && contains([]string{"OWNER", "MEMBER", "COLLABORATOR"}, r.Association) {
			i.HumanApproved = true
		}
	}
	return nil
}

// gh api --paginate emits consecutive JSON arrays. Decode every page without
// --slurp, which is unavailable in older supported gh installations. Reject
// empty/null output rather than treating an incomplete response as an empty
// repository and resetting observations.
func decodeGitHubPages[T any](raw string) ([][]T, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var pages [][]T
	for {
		var page []T
		err := dec.Decode(&page)
		if err == io.EOF {
			if len(pages) == 0 {
				return nil, fmt.Errorf("expected a JSON array page")
			}
			return pages, nil
		}
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("expected a JSON array page, got null")
		}
		pages = append(pages, page)
	}
}
