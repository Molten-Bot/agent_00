package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type GitHub struct{ Runner execx.Runner }

func (g GitHub) command(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	r, err := g.Runner.Run(ctx, execx.Command{Name: "gh", Args: args})
	if err != nil {
		return "", fmt.Errorf("GitHub %s: %w", args[0], err)
	}
	return r.Stdout, nil
}
func (g GitHub) Fetch(ctx context.Context, repo string) ([]Item, error) {
	if !repoPattern.MatchString(repo) {
		return nil, fmt.Errorf("invalid repository")
	}
	raw, err := g.command(ctx, "api", "--paginate", "repos/"+repo+"/issues?state=open&per_page=100")
	if err != nil {
		return nil, err
	}
	type issue struct {
		Number      int
		Title, Body string
		HTMLURL     string `json:"html_url"`
		User        struct{ Login, Type string }
		Labels      []struct{ Name string }
		Assignees   []struct{ Login string }
		Type        *struct{ Name string }
		PullRequest *json.RawMessage `json:"pull_request"`
	}
	pages, err := decodeGitHubPages[issue](raw)
	if err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	viewer, err := g.command(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return nil, err
	}
	viewer = strings.TrimSpace(viewer)
	items := []Item{}
	for _, page := range pages {
		for _, v := range page {
			if v.PullRequest != nil {
				i, err := g.Inspect(ctx, repo, strconv.Itoa(v.Number))
				if err != nil {
					return nil, err
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
			if v.Type != nil {
				i.IssueType = v.Type.Name
			}
			i.Viewer = viewer
			items = append(items, i)
		}
	}
	return items, nil
}
func (g GitHub) Inspect(ctx context.Context, repo, selector string) (Item, error) {
	if !repoPattern.MatchString(repo) {
		return Item{}, fmt.Errorf("invalid repository")
	}
	raw, err := g.command(ctx, "pr", "view", selector, "--repo", repo, "--json", "number,title,body,url,author,labels,assignees,reviewRequests,baseRefName,headRefName,headRefOid,isDraft,reviewDecision,statusCheckRollup,state")
	if err != nil {
		return Item{}, err
	}
	var p struct {
		Number                                                                        int
		Title, Body, URL, BaseRefName, HeadRefName, HeadRefOid, ReviewDecision, State string
		IsDraft                                                                       bool
		Author                                                                        struct {
			Login string
			IsBot bool
		}
		Labels            []struct{ Name string }
		Assignees         []struct{ Login string }
		ReviewRequests    []struct{ Login, Name, Slug string }
		StatusCheckRollup []struct{ Status, Conclusion, State string }
	}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return Item{}, err
	}
	if p.Number < 1 {
		return Item{}, fmt.Errorf("pull request response is missing its number")
	}
	// Older gh versions do not expose closingIssuesReferences in pr view's
	// JSON field list. Ask GraphQL directly for the relationship count.
	parts := strings.SplitN(repo, "/", 2)
	linked, err := g.command(ctx, "api", "graphql", "-f", "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){closingIssuesReferences{totalCount}}}}", "-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "number="+strconv.Itoa(p.Number), "--jq", ".data.repository.pullRequest.closingIssuesReferences.totalCount")
	if err != nil {
		return Item{}, err
	}
	linkedCount, err := strconv.Atoi(strings.TrimSpace(linked))
	if err != nil || linkedCount < 0 {
		return Item{}, fmt.Errorf("decode linked issue count")
	}
	i := Item{Repository: repo, Number: p.Number, Kind: "pr", Title: p.Title, Body: p.Body, URL: p.URL, Author: p.Author.Login, Bot: p.Author.IsBot, BaseBranch: p.BaseRefName, HeadBranch: p.HeadRefName, HeadSHA: p.HeadRefOid, Draft: p.IsDraft, LinkedIssue: linkedCount > 0, ReviewDecision: p.ReviewDecision, Open: p.State == "OPEN", ChecksPassed: len(p.StatusCheckRollup) > 0}
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
	for _, c := range p.StatusCheckRollup {
		state := c.Conclusion
		if state == "" {
			state = c.State
		}
		if !contains([]string{"SUCCESS", "NEUTRAL", "SKIPPED"}, state) {
			i.ChecksPassed = false
		}
		if contains([]string{"FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED"}, state) {
			i.ChecksFailed = true
		}
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
func (g GitHub) Merge(ctx context.Context, r Run) error {
	if r.MergeHead == "" {
		return fmt.Errorf("missing reviewed head")
	}
	args := []string{"pr", "merge", r.PRURL, "--repo", r.Item.Repository, "--" + r.Workflow.Merge.Method, "--match-head-commit", r.MergeHead}
	if r.Workflow.Merge.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	_, err := g.command(ctx, args...)
	return err
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
