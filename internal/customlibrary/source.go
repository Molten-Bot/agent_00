// Package customlibrary pulls company-owned library tasks from a private git
// repository's ./prompt/*.md files and converts them into the same
// library.TaskDefinition shape used by the built-in ./library/*.json catalog.
package customlibrary

import (
	"fmt"
	"regexp"
	"strings"
)

// Source is one configured external library repository.
type Source struct {
	// Name displays in the UI and namespaces every task generated from this
	// source, so two sources can never collide on task name.
	Name string `json:"name"`
	// Repo is any git remote git itself can clone: an https:// URL, an
	// ssh://, or a scp-like git@host:path form. Authentication is whatever
	// the host running this process already has configured (gh auth,
	// ssh-agent, credential helper) - no credentials are stored by this
	// package.
	Repo string `json:"repo"`
}

var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidateSource trims and checks a Source, returning a normalized copy.
func ValidateSource(src Source) (Source, error) {
	src.Name = strings.ToLower(strings.TrimSpace(src.Name))
	src.Repo = strings.TrimSpace(src.Repo)
	if !sourceNamePattern.MatchString(src.Name) {
		return Source{}, fmt.Errorf("name must be lowercase letters, numbers, and hyphens (max 48 characters)")
	}
	if src.Repo == "" {
		return Source{}, fmt.Errorf("repo is required")
	}
	if strings.ContainsAny(src.Repo, " \t\n\r") {
		return Source{}, fmt.Errorf("repo must not contain whitespace")
	}
	if strings.HasPrefix(src.Repo, "-") {
		return Source{}, fmt.Errorf("repo must not start with \"-\"")
	}
	return src, nil
}
