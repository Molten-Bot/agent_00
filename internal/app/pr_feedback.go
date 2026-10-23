package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

// ErrPRFeedbackTarget requires the caller to select or restore the original
// target, rather than queueing a code-repair task in another pull request.
var ErrPRFeedbackTarget = errors.New("PR feedback target unavailable")

var prFeedbackRequestPattern = regexp.MustCompile(`(?i)\b(fix|resolve|address)\b[^\n.!?]*\b(review|pr|pull.request)\b`)
var prFeedbackSubjectPattern = regexp.MustCompile(`(?i)\b(comments?|feedback|findings)\b|\bfix (this|the) (pr|pull request)\b`)

func normalizeLibraryReviewTarget(cfg *config.Config) error {
	if cfg.Review != nil || !isReviewOnlyRun(*cfg) {
		return nil
	}
	prURL := pullRequestURLFromPrompt(cfg.Prompt)
	head := normalizeBranchRef(cfg.BaseBranch)
	if prURL == "" && (head == "" || isProtectedDefaultBranchName(head)) {
		return fmt.Errorf("%w: code-review requires a PR URL or its existing head branch", ErrPRFeedbackTarget)
	}
	cfg.Review = &config.ReviewConfig{PRURL: prURL, HeadBranch: head, Writeback: "summary-comment"}
	// A URL is the explicit target; do not let the selected clone branch take
	// precedence in reviewSelector when both are present.
	if prURL != "" {
		cfg.Review.HeadBranch = ""
	}
	return nil
}

// Free-text feedback requests need the same branch reuse as the dedicated
// resolve-pr-comments task. Only inspect the opening request, not quoted task
// history in a failure follow-up or an unrelated library task.
func isFreeTextPRFeedbackRun(cfg config.Config) bool {
	if cfg.Review != nil || strings.TrimSpace(cfg.LibraryTaskName) != "" || isHarnessFailureFollowUpConfig(cfg) {
		return false
	}
	opening, _, _ := strings.Cut(strings.TrimSpace(cfg.Prompt), "\n")
	return prFeedbackRequestPattern.MatchString(opening) && prFeedbackSubjectPattern.MatchString(opening)
}

// Resolve the target before cloning or generating a branch. An unavailable or
// ambiguous target is an error, never a reason to open another PR.
func (h Harness) preparePRFeedbackTarget(ctx context.Context, cfg *config.Config) (string, error) {
	repos := cfg.RepoList()
	if len(repos) != 1 {
		return "", fmt.Errorf("PR feedback requires exactly one repository")
	}
	ref, ok := parseGitHubRepoRef(repos[0])
	if !ok {
		return "", fmt.Errorf("PR feedback requires a GitHub repository")
	}
	repoName := ref.owner + "/" + ref.name
	urls := regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`).FindAllString(cfg.Prompt, -1)
	selector := normalizeBranchRef(cfg.BaseBranch)
	if len(urls) > 0 {
		selector = urls[0]
		for _, u := range urls {
			if u != selector {
				return "", fmt.Errorf("PR feedback contains multiple pull request targets; select one PR")
			}
		}
		if !strings.EqualFold(githubutil.PullRequestRepository(selector), repoName) {
			return "", fmt.Errorf("PR feedback target does not belong to the configured repository")
		}
	} else if selector == "" || isProtectedDefaultBranchName(selector) {
		return "", fmt.Errorf("PR feedback requires a pull request URL or its existing head branch")
	}
	res, err := h.runCommand(ctx, "review", ghCommand("", "pr", "view", selector, "--repo", repoName,
		"--json", "url,state,headRefName,isCrossRepository"))
	if err != nil {
		return "", fmt.Errorf("resolve existing PR feedback target: %w", err)
	}
	var target struct {
		URL               string `json:"url"`
		State             string `json:"state"`
		HeadRefName       string `json:"headRefName"`
		IsCrossRepository bool   `json:"isCrossRepository"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &target); err != nil {
		return "", fmt.Errorf("decode PR feedback target: %w", err)
	}
	if !strings.EqualFold(target.State, "OPEN") || strings.TrimSpace(target.HeadRefName) == "" ||
		!strings.EqualFold(githubutil.PullRequestRepository(target.URL), repoName) {
		return "", fmt.Errorf("PR feedback target must be an open PR with a head branch in the configured repository")
	}
	if target.IsCrossRepository {
		return "", fmt.Errorf("PR feedback target uses a fork; updating its head repository is required, refusing a replacement PR")
	}
	if len(urls) == 0 && normalizeBranchRef(target.HeadRefName) != selector {
		return "", fmt.Errorf("PR feedback target head does not match the selected branch")
	}
	cfg.BaseBranch = target.HeadRefName
	return target.URL, nil
}
