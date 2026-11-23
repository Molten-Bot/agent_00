package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestApplyDefaultsUsesGitHubIssueTitleVerbatim(t *testing.T) {
	t.Parallel()

	const title = "agent00-Fix issue-20260407-002959"
	for _, issueTitle := range []string{title, ""} {
		cfg := Config{
			Prompt:      "Complete GitHub issue acme/repo#17: " + title + "\n\nIssue description",
			PRTitle:     "Stale PR title",
			GitHubIssue: &GitHubIssueConfig{Repository: "acme/repo", Number: 17, Title: issueTitle},
		}
		cfg.ApplyDefaults()
		cfg.ApplyDefaults()
		if cfg.PRTitle != title {
			t.Errorf("PRTitle = %q, want %q (issue metadata title %q)", cfg.PRTitle, title, issueTitle)
		}
	}
}

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

func TestNormalizePRBodyWithFencedPromptIsIdempotent(t *testing.T) {
	t.Parallel()
	prompt := "Run the example:\n\n```sh\necho example\n```\n\nKeep this final line."
	legacyBody := "Summary\n\nOriginal task prompt:\n```text\n" + prompt + "\n```"
	body := NormalizePRBody(legacyBody, prompt)
	if !strings.Contains(body, "Original task prompt:\n````text\n"+prompt+"\n````") || strings.Count(body, "Keep this final line.") != 1 {
		t.Fatalf("expected a single fenced prompt in %q", body)
	}
	if again := NormalizePRBody(body, prompt); again != body {
		t.Fatalf("second normalization changed body to %q", again)
	}
}

func TestNormalizePRBodyForGitHubIssueRemovesStaleLegacyFencedPrompt(t *testing.T) {
	t.Parallel()

	const summary = "Custom summary.\n\n```sh\necho summary\n```"
	const staleBlock = "Original task prompt:\n```text\nOld instructions.\n\n```sh\necho stale\n``` \t\n\nStale final line.\n``` \t"
	const prompt = "Use the updated instructions."
	issue := &GitHubIssueConfig{Number: 17, LinkMode: GitHubIssueLinkClose}
	for _, suffix := range []string{"", "\n\nCloses #17", "\n\nRelated to #17"} {
		body := summary + "\n\n" + staleBlock + suffix + "\n\n" + prBodyFooter
		want := NormalizePRBodyForGitHubIssue(summary+suffix, prompt, issue)
		got := NormalizePRBodyForGitHubIssue(body, prompt, issue)
		if got != want {
			t.Errorf("normalized body = %q, want %q", got, want)
		}
	}
}

func TestNormalizePRBodyMatchesWholeClosingFenceLine(t *testing.T) {
	t.Parallel()

	fence := strings.Repeat("`", 4096)
	body := "Summary before.\n\n" + prBodyPromptHeading + "\n" + fence + "text\n" +
		fence[:len(fence)-1] + "\n" + fence + "`\n" + fence + " extra\n" +
		"Stale final line.\n" + fence + " \t\n\nSummary after."
	got := NormalizePRBody(body, "Updated instructions.")
	want := NormalizePRBody("Summary before.\n\nSummary after.", "Updated instructions.")
	if got != want {
		t.Fatal("normalization closed the prompt at the wrong fence")
	}
}

func TestNormalizePRBodyForGitHubIssueFitsLongContent(t *testing.T) {
	t.Parallel()
	issue := &GitHubIssueConfig{Number: 17, LinkMode: GitHubIssueLinkClose}
	prompt := "Closes #17\n\n```sh\necho example\n```\n\n" + strings.Repeat("界", githubPRBodyMaxBytes)
	for _, summary := range []string{"Custom summary", strings.Repeat("a", githubPRBodyMaxBytes)} {
		normalized := NormalizePRBody(summary, prompt)
		body := NormalizePRBodyForGitHubIssue(normalized, prompt, issue)
		if len(body) > githubPRBodyMaxBytes || !utf8.ValidString(body) {
			t.Fatalf("invalid GitHub body of %d bytes", len(body))
		}
		if !strings.Contains(body, "[Prompt truncated to fit GitHub's body limit.]\n````") ||
			!strings.HasSuffix(body, "\n\nCloses #17\n\n"+prBodyFooter) {
			t.Fatal("body lost its closing fence, issue link, or footer")
		}
		if len(summary) < githubPRBodyMaxBytes && !strings.Contains(body, summary) {
			t.Fatal("body lost its custom summary")
		}
	}
}

func TestValidateGitHubIssueClaim(t *testing.T) {
	t.Parallel()
	issue := &GitHubIssueConfig{Repository: "acme/repo", Number: 17, URL: "https://github.com/acme/repo/issues/17", Claim: &GitHubIssueClaim{ID: "claim", Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: "claimed", Attempt: 1}}
	if err := validateGitHubIssueConfig(issue); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*GitHubIssueClaim){
		func(c *GitHubIssueClaim) { c.Branch = "main" },
		func(c *GitHubIssueClaim) { c.BaseBranch = c.Branch },
		func(c *GitHubIssueClaim) { c.Attempt = 0 },
		func(c *GitHubIssueClaim) { c.ExclusionLabel = "" },
	} {
		copy := *issue.Claim
		edit(&copy)
		changed := *issue
		changed.Claim = &copy
		if err := validateGitHubIssueConfig(&changed); err == nil {
			t.Fatalf("invalid claim accepted: %+v", copy)
		}
	}
}
