package localruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/library"
	"github.com/Molten-Bot/agent_00/internal/stringsx"
)

// ParseRunConfigJSON parses one inline run config JSON object into a validated config.
func ParseRunConfigJSON(payload []byte) (config.Config, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return config.Config{}, fmt.Errorf("run config payload is empty")
	}

	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return config.Config{}, fmt.Errorf("decode run config payload: %w", err)
	}
	return parseRunConfigValue(decoded)
}

func parseRunConfigValue(v any) (config.Config, error) {
	m, err := normalizeRunConfigMap(v)
	if err != nil {
		return config.Config{}, err
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		return config.Config{}, fmt.Errorf("marshal run config payload: %w", err)
	}

	var cfg config.Config
	dec := json.NewDecoder(bytes.NewReader(encoded))
	if err := dec.Decode(&cfg); err != nil {
		return config.Config{}, fmt.Errorf("decode run config payload: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("validate run config payload: %w", err)
	}
	return cfg, nil
}

func normalizeRunConfigMap(v any) (map[string]any, error) {
	var parsed map[string]any
	switch typed := v.(type) {
	case map[string]any:
		parsed = typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, fmt.Errorf("run config payload must be a JSON object")
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return nil, fmt.Errorf("decode run config payload string: %w", err)
		}
		m, ok := decoded.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("run config payload must be a JSON object")
		}
		parsed = m
	default:
		return nil, fmt.Errorf("run config payload must be a JSON object")
	}

	if err := normalizeRunConfigAliases(parsed); err != nil {
		return nil, err
	}
	if taskName := stringsx.FirstNonEmpty(stringAt(parsed, "libraryTaskName")); taskName != "" {
		return expandLibraryTaskRunConfig(parsed, taskName)
	}
	return parsed, nil
}

func stringAt(root map[string]any, key string) string {
	value, ok := root[key]
	if !ok {
		return ""
	}
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func stringAtAny(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAt(root, key); value != "" {
			return value
		}
	}
	return ""
}

func normalizeRunConfigAliases(m map[string]any) error {
	if m == nil {
		return fmt.Errorf("run config payload must be a JSON object")
	}

	copyRunConfigAlias(m, "repoUrl", "repourl")
	copyRunConfigAlias(m, "baseBranch", "basebranch")
	copyRunConfigAlias(m, "targetSubdir", "targetsubdir")
	copyRunConfigAlias(m, "responseMode", "responsemode")
	copyRunConfigAlias(m, "libraryTaskName", "librarytaskname")
	copyRunConfigAlias(m, "promptContext", "promptcontext")
	copyRunConfigAlias(m, "requiresNonDefaultBranch", "requiresnondefaultbranch")
	copyRunConfigAlias(m, "prNumber", "prnumber")
	copyRunConfigAlias(m, "prUrl", "prurl")
	copyRunConfigAlias(m, "headBranch", "headbranch")
	copyRunConfigAlias(m, "githubHandle", "githubhandle")
	copyRunConfigAlias(m, "agentHarness", "agentharness")
	copyRunConfigAlias(m, "agentCommand", "agentcommand")
	copyRunConfigAlias(m, "commitMessage", "commitmessage")
	copyRunConfigAlias(m, "prTitle", "prtitle")
	copyRunConfigAlias(m, "prBody", "prbody")
	normalizeReviewRunConfigAliases(m)

	if stringsx.FirstNonEmpty(stringAt(m, "baseBranch")) == "" {
		if branch := stringsx.FirstNonEmpty(stringAt(m, "branch")); branch != "" {
			m["baseBranch"] = branch
		}
	}
	if stringsx.FirstNonEmpty(stringAt(m, "prompt")) != "" && stringsx.FirstNonEmpty(stringAt(m, "libraryTaskName")) != "" {
		return fmt.Errorf("run config payload cannot include both prompt and librarytaskname")
	}
	if prNumber, ok := positiveIntValue(m["prNumber"]); ok {
		review := ensureReviewMap(m)
		if _, exists := review["prNumber"]; !exists {
			review["prNumber"] = prNumber
		}
	}
	if prURL := stringsx.FirstNonEmpty(stringAt(m, "prUrl")); prURL != "" {
		review := ensureReviewMap(m)
		if !runConfigAliasValueSet(review["prUrl"]) {
			review["prUrl"] = prURL
		}
	}
	if headBranch := stringsx.FirstNonEmpty(stringAt(m, "headBranch")); headBranch != "" {
		review := ensureReviewMap(m)
		if !runConfigAliasValueSet(review["headBranch"]) {
			review["headBranch"] = headBranch
		}
	}
	return nil
}

func copyRunConfigAlias(m map[string]any, canonical string, aliases ...string) {
	if m == nil || strings.TrimSpace(canonical) == "" {
		return
	}
	canonicalSet := runConfigAliasValueSet(m[canonical])
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		value, exists := m[alias]
		delete(m, alias)
		if canonicalSet || !exists || !runConfigAliasValueSet(value) {
			continue
		}
		m[canonical] = value
		canonicalSet = true
	}
}

func normalizeReviewRunConfigAliases(m map[string]any) {
	if m == nil {
		return
	}
	review, hasReview := m["review"].(map[string]any)
	if hasReview {
		copyRunConfigAlias(review, "prNumber", "prnumber")
		copyRunConfigAlias(review, "prUrl", "prurl")
		copyRunConfigAlias(review, "headBranch", "headbranch")
		copyRunConfigAlias(review, "notificationThreadId", "notificationthreadid")
		copyRunConfigAlias(review, "requestedReviewer", "requestedreviewer")
		copyRunConfigAlias(review, "requireRequestedReviewer", "requirerequestedreviewer")
		copyRunConfigAlias(review, "autoMerge", "automerge")
		copyRunConfigAlias(review, "mergeMethod", "mergemethod")
	}
	for _, source := range []struct {
		alias     string
		canonical string
	}{
		{alias: "review.prnumber", canonical: "prNumber"},
		{alias: "review.prurl", canonical: "prUrl"},
		{alias: "review.headbranch", canonical: "headBranch"},
		{alias: "review.trigger", canonical: "trigger"},
		{alias: "review.notificationthreadid", canonical: "notificationThreadId"},
		{alias: "review.requestedreviewer", canonical: "requestedReviewer"},
		{alias: "review.requirerequestedreviewer", canonical: "requireRequestedReviewer"},
		{alias: "review.writeback", canonical: "writeback"},
		{alias: "review.automerge", canonical: "autoMerge"},
		{alias: "review.mergemethod", canonical: "mergeMethod"},
	} {
		value, exists := m[source.alias]
		if !exists || !runConfigAliasValueSet(value) {
			continue
		}
		review := ensureReviewMap(m)
		if !runConfigAliasValueSet(review[source.canonical]) {
			review[source.canonical] = value
		}
	}
}

func runConfigAliasValueSet(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []string:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}

func ensureReviewMap(m map[string]any) map[string]any {
	if existing, ok := m["review"].(map[string]any); ok {
		return existing
	}
	review := map[string]any{}
	m["review"] = review
	return review
}

func positiveIntValue(v any) (int, bool) {
	switch typed := v.(type) {
	case int:
		return typed, typed > 0
	case int32:
		value := int(typed)
		return value, value > 0
	case int64:
		value := int(typed)
		return value, value > 0
	case float64:
		value := int(typed)
		return value, typed == float64(value) && value > 0
	case json.Number:
		value, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		intValue := int(value)
		return intValue, intValue > 0
	default:
		return 0, false
	}
}

func expandLibraryTaskRunConfig(m map[string]any, taskName string) (map[string]any, error) {
	repo := stringsx.FirstNonEmpty(stringAtAny(m, "repo", "repoUrl"))
	repos := nonEmptyStringArray(m["repos"])
	if repo == "" {
		if len(repos) == 1 {
			repo = repos[0]
		} else if len(repos) > 1 {
			repo = repos[0]
		}
	}

	catalog, err := library.LoadCustomizedCatalog(library.DefaultDir)
	if err != nil {
		return nil, fmt.Errorf("load library catalog: %w", err)
	}
	promptContext := stringAt(m, "promptContext")
	cfg, err := catalog.ExpandRunConfigWithContext(taskName, repo, stringsx.FirstNonEmpty(stringAt(m, "baseBranch"), stringAt(m, "branch")), promptContext)
	if err != nil {
		return nil, err
	}
	expanded, err := configToMap(cfg)
	if err != nil {
		return nil, err
	}
	delete(m, "promptContext")
	for key, value := range m {
		if isTrustedLibraryExpansionField(key) {
			continue
		}
		if _, exists := expanded[key]; exists {
			expanded[key] = value
			continue
		}
		if mapHasCaseInsensitiveKey(expanded, key) {
			continue
		}
		expanded[key] = value
	}
	if len(repos) > 0 {
		expanded["repos"] = repos
	}
	return expanded, nil
}

func mapHasCaseInsensitiveKey(m map[string]any, key string) bool {
	for existingKey := range m {
		if strings.EqualFold(existingKey, key) {
			return true
		}
	}
	return false
}

func isTrustedLibraryExpansionField(key string) bool {
	switch {
	case strings.EqualFold(key, "libraryTaskName"):
		return true
	case strings.EqualFold(key, "libraryTaskDisplayName"):
		return true
	case strings.EqualFold(key, "requiresNonDefaultBranch"):
		return true
	case strings.EqualFold(key, "prompt"):
		return true
	default:
		return false
	}
}

func configToMap(cfg config.Config) (map[string]any, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized run config: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("decode normalized run config: %w", err)
	}
	return out, nil
}

func nonEmptyStringArray(v any) []string {
	switch typed := v.(type) {
	case []string:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				out = append(out, entry)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			s, ok := entry.(string)
			if !ok {
				continue
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
