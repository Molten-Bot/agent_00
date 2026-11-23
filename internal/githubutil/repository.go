package githubutil

import "strings"

// RepositoryMatches compares a configured GitHub clone URL or OWNER/REPO
// shorthand with a repository identity returned by GitHub.
func RepositoryMatches(value, repository string) bool {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	parts := strings.Split(value, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && strings.EqualFold(value, repository)
}
