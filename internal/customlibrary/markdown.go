package customlibrary

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/library"
	"github.com/Molten-Bot/agent_00/internal/slug"
)

// loadPromptMarkdown converts every ./prompt/*.md file in dir into a
// library.TaskDefinition, namespaced under sourceName so tasks from
// different sources (or the built-in catalog) can never collide.
func loadPromptMarkdown(dir, sourceName string) ([]library.TaskDefinition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read prompt directory: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no .md library items found under %s", dir)
	}

	seen := make(map[string]struct{}, len(names))
	tasks := make([]library.TaskDefinition, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		task, err := taskFromMarkdown(sourceName, base, string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if _, dup := seen[task.Name]; dup {
			return nil, fmt.Errorf("%s: duplicate library task name %q", name, task.Name)
		}
		seen[task.Name] = struct{}{}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// taskFromMarkdown parses one prompt/*.md file. The file may start with a
// "---" delimited frontmatter block using the same flat key/value fields as
// library.TaskDefinition's JSON (name, displayName, type, icon, description,
// targetSubdir, commitMessage, prTitle, prBody, labels, githubHandle,
// reviewers, requiresNonDefaultBranch); everything after the closing "---"
// is the task prompt. A file with no frontmatter is treated as prompt text
// in full, with the task name derived from the filename.
func taskFromMarkdown(sourceName, fileBaseName, content string) (library.TaskDefinition, error) {
	frontmatter, body, hasFrontmatter := splitFrontmatter(content)

	scalars := map[string]string{}
	arrays := map[string][]string{}
	if hasFrontmatter {
		var err error
		scalars, arrays, err = parseFrontmatterFields(frontmatter)
		if err != nil {
			return library.TaskDefinition{}, err
		}
	}

	name := strings.TrimSpace(scalars["name"])
	if name == "" {
		name = slug.FromPrompt(fileBaseName)
	}

	displayName := strings.TrimSpace(scalars["displayName"])
	if displayName == "" {
		displayName = defaultCustomLibraryDisplayName(sourceName, name)
	}

	task := library.TaskDefinition{
		Name:                     sourceName + "--" + name,
		DisplayName:              displayName,
		Type:                     strings.TrimSpace(scalars["type"]),
		Icon:                     strings.TrimSpace(scalars["icon"]),
		Description:              strings.TrimSpace(scalars["description"]),
		TargetSubdir:             strings.TrimSpace(scalars["targetSubdir"]),
		Prompt:                   strings.TrimSpace(body),
		CommitMessage:            strings.TrimSpace(scalars["commitMessage"]),
		PRTitle:                  strings.TrimSpace(scalars["prTitle"]),
		PRBody:                   strings.TrimSpace(scalars["prBody"]),
		Labels:                   arrays["labels"],
		GitHubHandle:             strings.TrimSpace(scalars["githubHandle"]),
		Reviewers:                arrays["reviewers"],
		RequiresNonDefaultBranch: strings.EqualFold(strings.TrimSpace(scalars["requiresNonDefaultBranch"]), "true"),
	}
	if task.Prompt == "" {
		return library.TaskDefinition{}, fmt.Errorf("prompt body is required")
	}
	return task, nil
}

// defaultCustomLibraryDisplayName builds a display name from the source's
// configured name (the "Name" field set up when adding the library repo in
// settings) and the task's own slug, so a task without an explicit
// "displayName" frontmatter field still shows its source in the UI instead
// of the raw "sourceName--task-slug" internal task name.
func defaultCustomLibraryDisplayName(sourceName, name string) string {
	return titleCaseWords(sourceName) + ": " + titleCaseWords(name)
}

func titleCaseWords(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// splitFrontmatter separates a leading "---" delimited block from the rest
// of the document. ok is false when the document does not open with "---",
// in which case body is the entire, unmodified content.
func splitFrontmatter(content string) (frontmatter, body string, ok bool) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	leading := strings.TrimLeft(normalized, "\n")
	if !strings.HasPrefix(leading, "---") {
		return "", normalized, false
	}
	lines := strings.Split(leading, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", normalized, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", normalized, false
}

// parseFrontmatterFields parses flat "key: value" frontmatter lines. A value
// supports an inline array ("labels: [a, b]") or a YAML-style block list
// (a "key:" line followed by indented "- item" lines).
func parseFrontmatterFields(frontmatter string) (map[string]string, map[string][]string, error) {
	scalars := map[string]string{}
	arrays := map[string][]string{}

	lines := strings.Split(frontmatter, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			return nil, nil, fmt.Errorf("invalid frontmatter line %q", lines[i])
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, nil, fmt.Errorf("invalid frontmatter line %q", lines[i])
		}
		value = strings.TrimSpace(value)

		if value == "" {
			items, next := parseBlockList(lines, i+1)
			if len(items) > 0 {
				arrays[key] = items
				i = next - 1
				continue
			}
			scalars[key] = ""
			continue
		}

		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			arrays[key] = parseInlineArray(value)
			continue
		}

		scalars[key] = unquote(value)
	}
	return scalars, arrays, nil
}

func parseBlockList(lines []string, start int) ([]string, int) {
	var items []string
	i := start
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			i++
			continue
		}
		if trimmed == "-" {
			items = append(items, "")
			i++
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			items = append(items, unquote(strings.TrimSpace(trimmed[2:])))
			i++
			continue
		}
		break
	}
	return items, i
}

func parseInlineArray(value string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		items = append(items, unquote(strings.TrimSpace(part)))
	}
	return items
}

func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
