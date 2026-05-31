package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	backpressureFilename = "BACKPRESSURE.md"
	backpressureHeading  = "Backpressure completion contract:"
)

type backpressureRequirement struct {
	Label string
	Body  string
}

var defaultBackpressureRequirements = []string{
	"You should only consider the task done when every applicable criterion below is satisfied.",
	"Run available linting, formatting, type-checking, build, test, and simple verification scripts. Prefer focused checks while iterating, then broaden to the most relevant full checks before finishing.",
	"When the task affects a runnable API, service, or integration, perform a manual smoke check with the most direct local command available, such as curl against a local endpoint.",
	"When the task affects browser-visible behavior, run the app locally when practical and use Playwright or an actual browser path to verify the changed flow, capture screenshots when useful, and check for console errors.",
	"Run benchmarks or performance measurements only when repository evidence, existing benchmark scripts, or the task requirements make performance part of the acceptance criteria.",
	"Use review agents or explicit self-review passes where available. Cover functional correctness, tests, types, security/privacy, brevity/scope control, and visual design for UI work.",
	"If a planning phase exists for this run, treat plan review as a gate before implementation. If no separate planning phase exists, briefly plan, implement, and then review the final diff against the plan.",
	"Do not stop solely because optional tooling is missing in this runtime. Continue with alternative validation you can run and clearly report each validation gap in the final response.",
	"Remote pull-request creation, pull-request check monitoring, and CI remediation are managed by the harness after local work is complete.",
}

func withBackpressurePrompt(prompt string, requirements []backpressureRequirement) string {
	base := strings.TrimSpace(prompt)
	block := backpressurePromptBlock(requirements)
	if block == "" {
		return base
	}
	if strings.Contains(base, backpressureHeading) {
		return base
	}
	if base == "" {
		return block
	}
	return base + "\n\n" + block
}

func backpressurePromptBlock(requirements []backpressureRequirement) string {
	var b strings.Builder
	b.WriteString(backpressureHeading)
	b.WriteString("\n")
	for _, requirement := range defaultBackpressureRequirements {
		requirement = strings.TrimSpace(requirement)
		if requirement == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(requirement)
		b.WriteString("\n")
	}

	normalized := normalizeBackpressureRequirements(requirements)
	if len(normalized) > 0 {
		b.WriteString("\nProject-specific backpressure requirements:\n")
		for _, requirement := range normalized {
			label := strings.TrimSpace(requirement.Label)
			body := strings.TrimSpace(requirement.Body)
			if label == "" || body == "" {
				continue
			}
			b.WriteString(fmt.Sprintf("\nFrom %s:\n", label))
			b.WriteString(body)
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}

func normalizeBackpressureRequirements(requirements []backpressureRequirement) []backpressureRequirement {
	if len(requirements) == 0 {
		return nil
	}
	out := make([]backpressureRequirement, 0, len(requirements))
	seen := make(map[string]struct{}, len(requirements))
	for _, requirement := range requirements {
		label := strings.TrimSpace(requirement.Label)
		body := strings.TrimSpace(requirement.Body)
		if label == "" || body == "" {
			continue
		}
		key := label + "\x00" + body
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, backpressureRequirement{Label: label, Body: body})
	}
	return out
}

func (h Harness) collectBackpressureRequirements(repos []repoWorkspace, targetSubdir string) []backpressureRequirement {
	if len(repos) == 0 {
		return nil
	}

	requirements := make([]backpressureRequirement, 0, len(repos)+1)
	seen := map[string]struct{}{}
	addPath := func(path, label string) {
		path = filepath.Clean(strings.TrimSpace(path))
		label = strings.TrimSpace(label)
		if path == "" || label == "" {
			return
		}
		key := path
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}

		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				h.logf("stage=workspace status=warn action=load_backpressure path=%s err=%q", path, err)
			}
			return
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			return
		}
		requirements = append(requirements, backpressureRequirement{
			Label: label,
			Body:  body,
		})
	}

	if len(repos) == 1 {
		repo := repos[0]
		addPath(filepath.Join(repo.Dir, backpressureFilename), backpressureFilename)
		targetDir := filepath.Clean(filepath.Join(repo.Dir, strings.TrimSpace(targetSubdir)))
		if rel, err := filepath.Rel(repo.Dir, targetDir); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			addPath(filepath.Join(targetDir, backpressureFilename), filepath.ToSlash(filepath.Join(rel, backpressureFilename)))
		}
		return normalizeBackpressureRequirements(requirements)
	}

	for _, repo := range repos {
		label := filepath.ToSlash(filepath.Join(repo.RelDir, backpressureFilename))
		if strings.TrimSpace(repo.URL) != "" {
			label = fmt.Sprintf("%s (%s)", label, repo.URL)
		}
		addPath(filepath.Join(repo.Dir, backpressureFilename), label)
	}
	return normalizeBackpressureRequirements(requirements)
}
