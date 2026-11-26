package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

var ErrWorkflowPRMetadata = errors.New("workflow PR metadata update failed")

// IsWorkflowPRMetadataFailure recognizes the error after it has been stored
// as a task or claim outcome.
func IsWorkflowPRMetadataFailure(detail string) bool {
	return strings.HasPrefix(detail, "pr: "+ErrWorkflowPRMetadata.Error()+":")
}

func (h Harness) applyWorkflowPRMetadata(ctx context.Context, repo repoWorkspace, policy *config.WorkflowRunConfig) error {
	if policy == nil || (policy.PRAssignee == "" && len(policy.PRLabels) == 0) {
		return nil
	}
	repository := githubutil.PullRequestRepository(repo.PRURL)
	if !config.ValidGitHubRepository(repository) {
		return fmt.Errorf("%w: invalid pull request URL %q", ErrWorkflowPRMetadata, repo.PRURL)
	}
	endpoint := "repos/" + repository + "/issues/" + githubutil.PullRequestSelector(repo.PRURL)
	if policy.PRAssignee != "" {
		if err := h.addWorkflowPRMetadata(ctx, repo, endpoint+"/assignees", "assignee", "assignees", "[.assignees[].login]", policy.PRAssignee); err != nil {
			return err
		}
	}
	for _, label := range policy.PRLabels {
		if err := h.addWorkflowPRMetadata(ctx, repo, endpoint+"/labels", "label", "labels", "[.[].name]", label); err != nil {
			return err
		}
	}
	return nil
}

func (h Harness) addWorkflowPRMetadata(ctx context.Context, repo repoWorkspace, endpoint, kind, field, jq, value string) error {
	// POST adds values without replacing labels or assignees already on the PR.
	body, err := json.Marshal(map[string][]string{field: {value}})
	if err != nil {
		return err
	}
	cmd := ghCommand(repo.Dir, "api", "--method", "POST", endpoint, "--input", "-", "--jq", jq)
	cmd.Stdin = string(body)
	res, err := h.runCommand(ctx, "pr", cmd)
	description := fmt.Sprintf("add %s %q to existing PR %s", kind, value, repo.PRURL)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWorkflowPRMetadata, commandErrorWithDetails(
			description, err, res, maxGitErrorDetailChars))
	}

	// GitHub can silently ignore a login that cannot be assigned.
	var values []string
	if err := json.Unmarshal([]byte(res.Stdout), &values); err != nil {
		return fmt.Errorf("%w: verify %s %q on existing PR %s: %w", ErrWorkflowPRMetadata, kind, value, repo.PRURL, err)
	}
	if !slices.ContainsFunc(values, func(applied string) bool { return strings.EqualFold(applied, value) }) {
		return fmt.Errorf("%w: GitHub did not apply %s %q to existing PR %s", ErrWorkflowPRMetadata, kind, value, repo.PRURL)
	}
	return nil
}

func (h Harness) loadWorkflowPRToResume(ctx context.Context, cfg config.Config) (prStateView, error) {
	target := cfg.Workflow.ResumePRURL
	repos := cfg.RepoList()
	if len(repos) != 1 {
		return prStateView{}, fmt.Errorf("PR metadata retry requires one repository")
	}
	ref, ok := parseGitHubRepoRef(repos[0])
	repository := githubutil.PullRequestRepository(target)
	if !ok || !strings.EqualFold(repository, ref.owner+"/"+ref.name) {
		return prStateView{}, fmt.Errorf("PR metadata retry target must belong to the configured repository")
	}
	state, err := h.loadPullRequestState(ctx, "", target)
	if err != nil {
		return prStateView{}, err
	}
	if !pullRequestStateIsOpen(state) || state.HeadRefName == "" || state.BaseRefName == "" ||
		!strings.EqualFold(githubutil.PullRequestRepository(state.URL), repository) ||
		githubutil.PullRequestSelector(state.URL) != githubutil.PullRequestSelector(target) {
		return prStateView{}, fmt.Errorf("PR metadata retry requires the original pull request to be open")
	}
	if cfg.GitHubIssue != nil && cfg.GitHubIssue.Claim != nil {
		claim := cfg.GitHubIssue.Claim
		if state.HeadRefName != claim.Branch || state.BaseRefName != claim.BaseBranch {
			return prStateView{}, fmt.Errorf("PR metadata retry target must match the reserved issue branch and base")
		}
	}
	return state, nil
}

func workflowPRFork(state prStateView) string {
	fork := state.HeadRepository.NameWithOwner
	if fork == "" && state.HeadRepositoryOwner.Login != "" && state.HeadRepository.Name != "" {
		fork = state.HeadRepositoryOwner.Login + "/" + state.HeadRepository.Name
	}
	if strings.EqualFold(fork, githubutil.PullRequestRepository(state.URL)) {
		return ""
	}
	return fork
}

func (h Harness) checkoutWorkflowPRFork(ctx context.Context, repo *repoWorkspace, state prStateView, fork string) error {
	if !config.ValidGitHubRepository(fork) {
		return fmt.Errorf("PR metadata retry received an invalid fork repository")
	}
	forkURL := "https://github.com/" + fork + ".git"
	if isGitHubSSHRemoteURL(repo.URL) && !hasGitHubAuthToken() {
		forkURL = "git@github.com:" + fork + ".git"
	}
	if err := h.ensureForkRemoteConfigured(ctx, *repo, forkURL); err != nil {
		return err
	}
	ref := "refs/heads/" + state.HeadRefName
	if _, err := h.runCommand(ctx, "git", fetchBranchFromRemoteCommand(repo.Dir, publishRemoteFork, ref)); err != nil {
		return err
	}
	if _, err := h.runCommand(ctx, "git", gitCommand(repo.Dir, "checkout", "-B", state.HeadRefName, "FETCH_HEAD")); err != nil {
		return err
	}
	repo.PushRemote = publishRemoteFork
	repo.PublishStrategy = publishStrategyForkFallback
	repo.PRTargetRepo = githubutil.PullRequestRepository(state.URL)
	repo.PRHeadOwner, _, _ = strings.Cut(fork, "/")
	repo.PRHeadRef = repo.PRHeadOwner + ":" + state.HeadRefName
	return nil
}
