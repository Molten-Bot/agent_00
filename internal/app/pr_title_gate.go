package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

// Pull-request title gates validate PR metadata instead of repository files.
// A remediation agent cannot clear them from the worktree, so the harness
// repairs the title it supplied to `gh pr create` itself.

const (
	semanticPullRequestAction = "action-semantic-pull-request"
	// workflowsRelDir is the repository-relative path GitHub reads workflows from.
	workflowsRelDir = ".github/workflows"
)

// prTitleGate describes a pull-request title policy declared by a repository
// workflow step that runs the semantic-pull-request action.
type prTitleGate struct {
	WorkflowPath string
	Types        []string
	Scopes       []string
	RequireScope bool
}

// preferredPRTitleTypes orders the conventional-commit types the harness picks
// from when the current title type is rejected by the gate.
var preferredPRTitleTypes = []string{"chore", "fix", "feat", "refactor", "build", "ci", "docs", "perf", "style", "test"}

var conventionalPRTitleRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*)(?:\(([^()]*)\))?!?:\s*(.+)$`)

var regexMetaRE = regexp.MustCompile(`[\\^$.|?*+()\[\]{}]`)

var checkNameWordRE = regexp.MustCompile(`[a-z0-9]+`)

// isPRTitleGateCheckName reports whether a failing check name looks like a
// pull-request title gate. A name must carry a "title" word plus a gate word,
// matched on word boundaries so checks such as "Sprint title sync" or a bare
// "Title" are not mistaken for pull-request metadata gates.
func isPRTitleGateCheckName(name string) bool {
	title, gate := false, false
	for _, word := range checkNameWordRE.FindAllString(strings.ToLower(name), -1) {
		switch word {
		case "title", "titles":
			title = true
		case "pr", "prs", "pull", "check", "checks", "lint", "semantic", "conventional", "validate", "verify":
			gate = true
		}
	}
	return title && gate
}

// checkSummaryBlocksOnPRTitleGateOnly reports whether the captured check output
// has failing checks and every one of them is a title gate.
func checkSummaryBlocksOnPRTitleGateOnly(summary string) bool {
	states := checkStatesFromText(summary)
	if len(states) == 0 {
		return false
	}
	titleGateFailed := false
	for name, state := range states {
		if !isFailedCheckBucket(state.Bucket) {
			continue
		}
		if !isPRTitleGateCheckName(name) {
			return false
		}
		titleGateFailed = true
	}
	return titleGateFailed
}

// repairPRTitleGateFailure rewrites the pull-request title when the only
// failing checks are title gates. It reports whether the title was changed;
// editing the title re-runs the gate because those workflows trigger on the
// pull_request `edited` event, which a plain push does not raise.
func (h Harness) repairPRTitleGateFailure(ctx context.Context, repo repoWorkspace, cfg config.Config, checkSummary string) (bool, error) {
	if strings.TrimSpace(repo.PRURL) == "" || !checkSummaryBlocksOnPRTitleGateOnly(checkSummary) {
		return false, nil
	}
	gate, found := detectPRTitleGate(repo.Dir)
	if !found {
		return false, nil
	}

	current, err := h.pullRequestTitle(ctx, repo)
	if err != nil {
		return false, err
	}
	if current == "" {
		current = strings.TrimSpace(cfg.PRTitle)
	}
	rewritten, ok := conformingPRTitle(current, gate, githubutil.PullRequestNumber(repo.PRURL))
	if !ok {
		return false, nil
	}

	if _, err := h.runCommand(ctx, "pr", prEditTitleCommand(repo.Dir, repo.PRURL, rewritten)); err != nil {
		return false, err
	}
	h.logf(
		"stage=pr status=ok action=retitle reason=title_gate repo=%s repo_dir=%s pr_url=%s workflow=%s",
		repo.URL,
		repo.RelDir,
		repo.PRURL,
		gate.WorkflowPath,
	)
	return true, nil
}

func (h Harness) pullRequestTitle(ctx context.Context, repo repoWorkspace) (string, error) {
	res, err := h.runCommand(ctx, "pr", prTitleViewCommand(repo.Dir, repo.PRURL))
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" {
		return "", nil
	}
	var view struct {
		Title string `json:"title"`
	}
	if decodeErr := json.Unmarshal([]byte(raw), &view); decodeErr != nil {
		return "", fmt.Errorf("decode pull request title: %w", decodeErr)
	}
	return strings.TrimSpace(view.Title), nil
}

func prTitleViewCommand(repoDir, prURL string) execx.Command {
	args := []string{"pr", "view", githubutil.PullRequestSelector(prURL), "--json", "title"}
	if repo := githubutil.PullRequestRepository(prURL); repo != "" {
		args = append(args, "--repo", repo)
	}
	return ghCommand(repoDir, args...)
}

func prEditTitleCommand(repoDir, prURL, title string) execx.Command {
	args := []string{"pr", "edit", githubutil.PullRequestSelector(prURL), "--title", title}
	if repo := githubutil.PullRequestRepository(prURL); repo != "" {
		args = append(args, "--repo", repo)
	}
	return ghCommand(repoDir, args...)
}

// detectPRTitleGate reads the checked-out workflows and returns the first
// semantic-pull-request title policy it can parse.
func detectPRTitleGate(repoDir string) (prTitleGate, bool) {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return prTitleGate{}, false
	}
	dir := filepath.Join(repoDir, filepath.FromSlash(workflowsRelDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return prTitleGate{}, false
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".yml", ".yaml":
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			continue
		}
		gate, ok := parsePRTitleGate(string(data))
		if !ok {
			continue
		}
		gate.WorkflowPath = workflowsRelDir + "/" + name
		return gate, true
	}
	return prTitleGate{}, false
}

// parsePRTitleGate scans workflow text for a semantic-pull-request step and
// reads its `with:` inputs. The scan is indentation aware rather than a full
// YAML parse because only this one action shape has to be understood.
func parsePRTitleGate(content string) (prTitleGate, bool) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		usesAt := semanticPullRequestUsesIndent(line)
		if usesAt < 0 {
			continue
		}
		gate, ok := parsePRTitleGateStep(lines, i+1, usesAt)
		if !ok {
			// The step exists but declares no inputs; the gate still applies
			// with the action's default type list.
			return prTitleGate{}, true
		}
		return gate, true
	}
	return prTitleGate{}, false
}

// semanticPullRequestUsesIndent returns the column of the `uses:` key when the
// line pins the semantic-pull-request action, or -1 otherwise.
func semanticPullRequestUsesIndent(line string) int {
	if !strings.Contains(line, semanticPullRequestAction) {
		return -1
	}
	at := strings.Index(line, "uses:")
	if at < 0 {
		return -1
	}
	if strings.TrimSpace(line[:at]) != "" && strings.TrimSpace(line[:at]) != "-" {
		return -1
	}
	return at
}

func parsePRTitleGateStep(lines []string, start, keyIndent int) (prTitleGate, bool) {
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		indent := lineIndent(lines[j])
		if indent < keyIndent {
			return prTitleGate{}, false
		}
		if indent == keyIndent && strings.TrimSpace(lines[j]) == "with:" {
			return parsePRTitleGateInputs(lines, j+1, keyIndent), true
		}
	}
	return prTitleGate{}, false
}

func parsePRTitleGateInputs(lines []string, start, keyIndent int) prTitleGate {
	var gate prTitleGate
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		indent := lineIndent(lines[j])
		if indent <= keyIndent {
			break
		}
		key, rest, ok := strings.Cut(strings.TrimSpace(lines[j]), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		switch key {
		case "types":
			gate.Types = parseWorkflowScalarList(lines, j, indent, rest)
		case "scopes":
			gate.Scopes = parseWorkflowScalarList(lines, j, indent, rest)
		case "requireScope":
			gate.RequireScope = isTruthyWorkflowValue(rest)
		}
	}
	return gate
}

// parseWorkflowScalarList reads an inline flow sequence, a single scalar, or a
// block scalar / dash list nested under the key at the given indent.
func parseWorkflowScalarList(lines []string, keyLine, keyIndent int, rest string) []string {
	rest = strings.TrimSpace(rest)
	if rest != "" && !isBlockScalarMarker(rest) {
		if strings.HasPrefix(rest, "[") {
			return splitWorkflowScalars(strings.Split(strings.Trim(rest, "[]"), ","))
		}
		return splitWorkflowScalars([]string{rest})
	}

	var raw []string
	for j := keyLine + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if lineIndent(lines[j]) <= keyIndent {
			break
		}
		raw = append(raw, strings.TrimPrefix(strings.TrimSpace(lines[j]), "- "))
	}
	return splitWorkflowScalars(raw)
}

func splitWorkflowScalars(raw []string) []string {
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		// Drop whole-line YAML comments. Issue-style scope patterns such as
		// `#\d+` start with `#` too, so only a `# ` prefix counts as a comment.
		if item == "" || item == "#" || strings.HasPrefix(item, "# ") {
			continue
		}
		item = strings.TrimSpace(strings.Trim(item, `"'`))
		if item == "" {
			continue
		}
		values = append(values, item)
	}
	return values
}

func isBlockScalarMarker(rest string) bool {
	switch strings.TrimSpace(rest) {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

func isTruthyWorkflowValue(value string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`)) {
	case "true", "yes", "on":
		return true
	}
	return false
}

func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// conformingPRTitle rewrites title so it satisfies gate. It reports false when
// the title already conforms or no conforming title can be derived without
// guessing a repository-specific scope.
func conformingPRTitle(title string, gate prTitleGate, prNumber int) (string, bool) {
	current := strings.TrimSpace(title)
	currentType, currentScope, subject := splitConventionalPRTitle(current)
	if subject == "" {
		return "", false
	}

	newType, ok := conformingPRTitleType(currentType, gate.Types)
	if !ok {
		return "", false
	}
	newScope, ok := conformingPRTitleScope(currentScope, gate, prNumber)
	if !ok {
		return "", false
	}

	rewritten := newType
	if newScope != "" {
		rewritten += "(" + newScope + ")"
	}
	rewritten += ": " + subject
	if rewritten == current {
		return "", false
	}
	return rewritten, true
}

func splitConventionalPRTitle(title string) (typ, scope, subject string) {
	title = strings.TrimSpace(title)
	match := conventionalPRTitleRE.FindStringSubmatch(title)
	if match == nil {
		return "", "", title
	}
	return match[1], strings.TrimSpace(match[2]), strings.TrimSpace(match[3])
}

func conformingPRTitleType(current string, allowed []string) (string, bool) {
	if len(allowed) == 0 {
		if current != "" {
			return current, true
		}
		return preferredPRTitleTypes[0], true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, current) {
			return candidate, true
		}
	}
	for _, preferred := range preferredPRTitleTypes {
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, preferred) {
				return candidate, true
			}
		}
	}
	return allowed[0], true
}

func conformingPRTitleScope(current string, gate prTitleGate, prNumber int) (string, bool) {
	if current != "" && matchesGateScope(current, gate.Scopes) {
		return current, true
	}
	if !gate.RequireScope {
		// The gate tolerates a missing scope, so drop the rejected one rather
		// than invent a repository-specific replacement.
		return "", true
	}

	var candidates []string
	if prNumber > 0 {
		candidates = append(candidates, "#"+strconv.Itoa(prNumber))
	}
	candidates = append(candidates, "deps")
	for _, scope := range gate.Scopes {
		if !regexMetaRE.MatchString(scope) {
			candidates = append(candidates, scope)
		}
	}
	for _, candidate := range candidates {
		if matchesGateScope(candidate, gate.Scopes) {
			return candidate, true
		}
	}
	return "", false
}

func matchesGateScope(scope string, patterns []string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
	}
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !regexMetaRE.MatchString(pattern) {
			if pattern == scope {
				return true
			}
			continue
		}
		compiled, err := regexp.Compile(`^(?:` + pattern + `)$`)
		if err != nil {
			continue
		}
		if compiled.MatchString(scope) {
			return true
		}
	}
	return false
}
