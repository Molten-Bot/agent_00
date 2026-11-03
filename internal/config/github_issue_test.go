package config

import (
	"strings"
	"testing"
)

func TestValidateGitHubIssueRunMetadata(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Repo:   "https://github.com/acme/repo",
		Prompt: "Implement issue",
		GitHubIssue: &GitHubIssueConfig{
			Repository: "acme/repo",
			Number:     17,
			URL:        "https://github.com/acme/repo/issues/17",
			LinkMode:   GitHubIssueLinkClose,
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want valid issue-originated run", err)
	}

	for _, test := range []struct {
		name string
		edit func(*GitHubIssueConfig)
		want string
	}{
		{"repository", func(issue *GitHubIssueConfig) { issue.Repository = "../repo" }, "OWNER/REPO"},
		{"number", func(issue *GitHubIssueConfig) { issue.Number = 0 }, "positive"},
		{"url", func(issue *GitHubIssueConfig) { issue.URL = "https://github.com/acme/other/issues/17" }, "identify"},
		{"link mode", func(issue *GitHubIssueConfig) { issue.LinkMode = "delete" }, "reference or close"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := cfg
			issue := *cfg.GitHubIssue
			test.edit(&issue)
			invalid.GitHubIssue = &issue
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestNormalizePRBodyForGitHubIssueAddsConfiguredLinkBeforeFooter(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		mode string
		want string
	}{
		{GitHubIssueLinkReference, "Related to #17"},
		{GitHubIssueLinkClose, "Closes #17"},
		{"", "Related to #17"},
	} {
		body := NormalizePRBodyForGitHubIssue("Summary", "Implement issue", &GitHubIssueConfig{
			Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17", LinkMode: test.mode,
		})
		linkAt := strings.Index(body, test.want)
		footerAt := strings.Index(body, prBodyFooter)
		if linkAt < 0 || footerAt < 0 || linkAt >= footerAt {
			t.Fatalf("normalized body = %q, want %q before the footer", body, test.want)
		}
	}
}
