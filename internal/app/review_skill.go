package app

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

const reviewSkillRelativePath = "skills/review/SKILL.md"

func withReviewSkillPrompt(prompt string) (string, error) {
	path, err := resolveReviewSkillPath()
	if err != nil {
		return "", fmt.Errorf("load review skill instructions: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	body := stripSkillFrontMatter(string(data))
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("skill file %s is empty", path)
	}
	parts := []string{
		"The bundled review skill is mandatory for this read-only review pass:",
		body,
		strings.TrimSpace(prompt),
	}
	return strings.TrimSpace(strings.Join(compactNonEmptyStrings(parts), "\n\n")), nil
}

func resolveReviewSkillPath() (string, error) {
	for _, candidate := range reviewSkillPathCandidates() {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("unable to find %s", reviewSkillRelativePath)
}

func reviewSkillPathCandidates() []string {
	candidates := []string{
		reviewSkillRelativePath,
		filepath.Join(workspaceSkillsDir, "review", "SKILL.md"),
		filepath.Join(runtimeSkillsDir, "review", "SKILL.md"),
	}
	if seedPath := strings.TrimSpace(os.Getenv("HARNESS_AGENTS_SEED_PATH")); seedPath != "" {
		baseDir := filepath.Dir(filepath.Dir(seedPath))
		candidates = append(candidates, filepath.Join(baseDir, "skills", "review", "SKILL.md"))
	}
	if _, file, _, ok := goruntime.Caller(0); ok {
		repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		candidates = append(candidates, filepath.Join(repoRoot, reviewSkillRelativePath))
	}

	seen := make(map[string]struct{}, len(candidates))
	deduped := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		deduped = append(deduped, candidate)
	}
	return deduped
}
