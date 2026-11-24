package web

import (
	"context"
	"errors"
	"testing"
)

func TestRepositoryLoadMigratesWorkflowsBeforePublishingRepositories(t *testing.T) {
	t.Parallel()
	srv := NewServer("", NewBroker())
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) { return []GitHubRepo{{FullName: "acme/repo"}}, nil }
	calls := 0
	srv.MigrateWorkflowDefaults = func(repos []GitHubRepo) error {
		calls++
		if len(repos) != 1 || repos[0].FullName != "acme/repo" {
			t.Fatal(repos)
		}
		if calls == 1 {
			return errors.New("temporary migration failure")
		}
		return nil
	}
	if _, err := srv.resolveGitHubRepos(context.Background()); err == nil {
		t.Fatal("migration error ignored")
	}
	repos, err := srv.resolveGitHubRepos(context.Background())
	if err != nil || calls != 2 || len(repos) != 1 {
		t.Fatal(repos, calls, err)
	}
}
