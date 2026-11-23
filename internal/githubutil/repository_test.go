package githubutil

import "testing"

func TestRepositoryMatches(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"acme/repo", "git@github.com:acme/repo.git", "ssh://git@github.com/acme/repo.git", " https://github.com/ACME/repo.git/ "} {
		if !RepositoryMatches(value, "acme/repo") {
			t.Errorf("rejected GitHub repository %q", value)
		}
	}
	for _, value := range []string{"", "https://other.test/acme/repo", "https://github.com.evil.test/acme/repo", "git@other.test:acme/repo.git", "acme/repo/extra", "https://github.com/git@github.com:acme/repo.git", "other/repo"} {
		if RepositoryMatches(value, "acme/repo") {
			t.Errorf("accepted unrelated repository %q", value)
		}
	}
}
