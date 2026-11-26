package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

// Use the same gh account and credential selection as issue admission.
func (s Server) issueGitHubRepos(ctx context.Context) ([]GitHubRepo, error) {
	result, err := s.runIssueGH(githubutil.WithCaller(ctx, "repository_catalog"), execx.Command{Name: "gh", Args: []string{"api", "--paginate", "user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=pushed"}})
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	var repos []GitHubRepo
	for {
		var page []struct {
			GitHubRepo
			Owner struct {
				Type      string `json:"type"`
				AvatarURL string `json:"avatar_url"`
			} `json:"owner"`
		}
		if err := decoder.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode GitHub repository page: %w", err)
		}
		for _, entry := range page {
			repo := entry.GitHubRepo
			repo.OwnerType = entry.Owner.Type
			repo.OwnerAvatarURL = entry.Owner.AvatarURL
			kind := repositoryOwnerKind(repo.OwnerType)
			repo.OwnerKind = string(kind)
			repo.Public = !repo.Private
			repo.Personal = kind == RepositoryOwnerPersonal
			repo.Organization = kind == RepositoryOwnerOrganization
			repo.Visibility = repositoryVisibilityString(repo.Private)
			repos = append(repos, repo)
		}
	}
	sortGitHubReposByActivity(repos)
	return repos, nil
}
