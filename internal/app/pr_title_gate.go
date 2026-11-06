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
	WorkflowPath   string
	Types          []string
	Scopes         []string
	DisallowScopes []string
	SubjectPattern string
	RequireScope   bool
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
//
// A commit word without a pull-request word rules the name out: a commit-title
// linter grades commit messages, which retitling the pull request cannot fix.
func isPRTitleGateCheckName(name string) bool {
	title, gate, pullRequest, commit := false, false, false, false
	for _, word := range checkNameWordRE.FindAllString(strings.ToLower(name), -1) {
		switch word {
		case "title", "titles":
			title = true
		case "pr", "prs", "pull":
			pullRequest, gate = true, true
		case "check", "checks", "lint", "semantic", "conventional", "validate", "verify":
			gate = true
		case "commit", "commits":
			commit = true
		}
	}
	if commit && !pullRequest {
		return false
	}
	return title && gate
}

// prTitleGateOnlyFailure reports whether the observed check results have
// failures and every failing check is a title gate.
//
// Both the raw watch output and the summary are scanned. The summary can carry
// a snapshot taken after the watch output was captured, so every non-gate check
// resolves to the freshest state reported for it and a stale red row cannot
// veto the repair on its own. That summary is truncated at maxCheckSummaryChars,
// so a row it drops, or cuts down to an unrecognized bucket, keeps the state the
// watch captured. A gate failure seen in any source still counts, because the
// watch can be the only source that observed the gate run.
func prTitleGateOnlyFailure(res execx.Result, summary string) bool {
	titleGateFailed := false
	others := make(map[string]string)
	for _, raw := range []string{res.Stdout, res.Stderr, summary} {
		for name, state := range checkStatesFromText(raw) {
			if isPRTitleGateCheckName(name) {
				titleGateFailed = titleGateFailed || isFailedCheckBucket(state.Bucket)
				continue
			}
			if _, seen := others[name]; seen && !isKnownCheckBucket(state.Bucket) {
				continue
			}
			others[name] = state.Bucket
		}
	}
	for _, bucket := range others {
		if isFailedCheckBucket(bucket) {
			return false
		}
	}
	return titleGateFailed
}

// repairPRTitleGateFailure rewrites the pull-request title when the only
// failing checks are title gates. It reports whether the title was changed;
// editing the title re-runs the gate because those workflows subscribe to the
// pull_request `edited` event, which a plain push does not raise.
//
// Only a pull request the harness owns is eligible; a pre-existing or
// PR-feedback target belongs to its author.
func (h Harness) repairPRTitleGateFailure(
	ctx context.Context,
	repo repoWorkspace,
	issue *config.GitHubIssueConfig,
	checkRes execx.Result,
	checkSummary string,
) (bool, error) {
	if !harnessCanRetitlePR(repo) {
		return false, nil
	}
	if !prTitleGateOnlyFailure(checkRes, checkSummary) {
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
		// Without the live title there is nothing to conform; a configured
		// title may belong to a different pull request on this branch.
		return false, nil
	}
	rewritten, ok := conformingPRTitle(current, gate, prTitleScopeNumbers(repo.PRURL, issue))
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

// harnessCanRetitlePR reports whether the harness may rewrite the title of the
// pull request it is looking at. Callers that would otherwise punish a red
// title gate must consult it first: a gate on a pull request the harness does
// not own is never repaired, so treating it as repairable only fails a run.
func harnessCanRetitlePR(repo repoWorkspace) bool {
	return strings.TrimSpace(repo.PRURL) != "" && harnessOwnsPRTitle(repo)
}

// prTitleScopeNumbers returns the issue-style scope numbers the harness can
// state as fact, most specific first. A run started from a GitHub issue in the
// same repository scopes the title to that issue; the pull-request number is a
// self-referential fallback used only when no issue backs the run.
func prTitleScopeNumbers(prURL string, issue *config.GitHubIssueConfig) []int {
	var numbers []int
	if issue != nil && issue.Number > 0 && issueBelongsToPullRequestRepo(issue.Repository, prURL) {
		numbers = append(numbers, issue.Number)
	}
	if prNumber := githubutil.PullRequestNumber(prURL); prNumber > 0 {
		numbers = append(numbers, prNumber)
	}
	return numbers
}

// issueBelongsToPullRequestRepo reports whether an OWNER/REPO issue repository
// is the repository the pull request lives in. A `#<number>` scope resolves
// against that repository, so a foreign issue number must never be stamped.
func issueBelongsToPullRequestRepo(issueRepo, prURL string) bool {
	issueRepo = strings.TrimSpace(issueRepo)
	prRepo := githubutil.PullRequestRepository(prURL)
	return issueRepo != "" && prRepo != "" && strings.EqualFold(issueRepo, prRepo)
}

// harnessOwnsPRTitle reports whether the harness supplied the pull-request
// title it is about to rewrite. That covers a pull request this run created
// and one an earlier run opened for the same harness-generated branch, which
// a re-run adopts through lookupOpenPRURLByHead. A PR-feedback target is never
// owned, and neither is a pull request on a branch the harness did not name.
func harnessOwnsPRTitle(repo repoWorkspace) bool {
	if repo.RequireExistingPR {
		return false
	}
	return repo.PRCreatedThisRun || isHarnessGeneratedBranch(repo.Branch)
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
//
// A workflow that does not subscribe to the pull_request `edited` event is not
// reported: retitling would never re-run it, so the gate would stay red.
func parsePRTitleGate(content string) (prTitleGate, bool) {
	lines := strings.Split(content, "\n")
	if !workflowRerunsOnTitleEdit(lines) {
		return prTitleGate{}, false
	}
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

// workflowRerunsOnTitleEdit reports whether the workflow asks for the
// pull_request (or pull_request_target) `edited` event. GitHub's default event
// types are opened, synchronize, and reopened, so a workflow that omits
// `edited` never re-runs when only the title changes.
func workflowRerunsOnTitleEdit(lines []string) bool {
	for i, line := range lines {
		if lineIndent(line) != 0 || isWorkflowCommentLine(line) {
			continue
		}
		key, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.Trim(strings.TrimSpace(key), `"'`) != "on" {
			continue
		}
		if strings.TrimSpace(rest) != "" {
			// `on: pull_request` and `on: [pull_request]` keep the default
			// event types, which exclude `edited`.
			return false
		}
		return pullRequestTriggerIncludesEdited(lines, i+1)
	}
	return false
}

// pullRequestTriggerIncludesEdited scans the mapping under a workflow `on:` key
// for a pull_request trigger whose `types` list names `edited`.
func pullRequestTriggerIncludesEdited(lines []string, start int) bool {
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" || isWorkflowCommentLine(lines[j]) {
			continue
		}
		indent := lineIndent(lines[j])
		if indent == 0 {
			return false
		}
		key, rest, ok := strings.Cut(strings.TrimSpace(lines[j]), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "pull_request", "pull_request_target":
		default:
			continue
		}
		if strings.TrimSpace(rest) != "" {
			continue
		}
		if eventTypesIncludeEdited(lines, j+1, indent) {
			return true
		}
	}
	return false
}

func eventTypesIncludeEdited(lines []string, start, eventIndent int) bool {
	for j := start; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" || isWorkflowCommentLine(lines[j]) {
			continue
		}
		indent := lineIndent(lines[j])
		if indent <= eventIndent {
			return false
		}
		key, rest, ok := strings.Cut(strings.TrimSpace(lines[j]), ":")
		if !ok || strings.TrimSpace(key) != "types" {
			continue
		}
		for _, eventType := range parseWorkflowScalarList(lines, j, indent, rest) {
			if strings.EqualFold(eventType, "edited") {
				return true
			}
		}
		return false
	}
	return false
}

func isWorkflowCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
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
		case "disallowScopes":
			gate.DisallowScopes = parseWorkflowScalarList(lines, j, indent, rest)
		case "subjectPattern":
			gate.SubjectPattern = parseWorkflowScalar(lines, j, indent, rest)
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

// parseWorkflowScalar reads a single scalar input, following a block scalar
// onto the line nested under the key. Unlike parseWorkflowScalarList it never
// splits the value: a regex input may start with `[` or contain commas.
func parseWorkflowScalar(lines []string, keyLine, keyIndent int, rest string) string {
	if rest = strings.TrimSpace(rest); rest != "" && !isBlockScalarMarker(rest) {
		return unquoteWorkflowScalar(rest)
	}
	for j := keyLine + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if lineIndent(lines[j]) <= keyIndent {
			break
		}
		return unquoteWorkflowScalar(strings.TrimSpace(lines[j]))
	}
	return ""
}

func unquoteWorkflowScalar(value string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
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

// conformingPRTitle rewrites title so it satisfies gate, drawing a synthesized
// scope from scopeNumbers in order. It reports false when the title already
// conforms or no conforming title can be derived without guessing a
// repository-specific scope or rewording the subject.
func conformingPRTitle(title string, gate prTitleGate, scopeNumbers []int) (string, bool) {
	current := strings.TrimSpace(title)
	currentType, currentScope, subject := splitConventionalPRTitle(current)
	if subject == "" {
		return "", false
	}
	if !subjectMatchesGatePattern(subject, gate.SubjectPattern) {
		// The gate also grades the subject, which is the operator's wording.
		// Rewriting only the type and scope would leave the gate red while the
		// harness reported the title repaired, so leave the title alone.
		return "", false
	}

	newType, ok := conformingPRTitleType(currentType, gate.Types)
	if !ok {
		return "", false
	}
	newScope, ok := conformingPRTitleScope(currentScope, gate, scopeNumbers)
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

func conformingPRTitleScope(current string, gate prTitleGate, scopeNumbers []int) (string, bool) {
	if current != "" && gateAllowsScope(current, gate) {
		return current, true
	}
	if !gate.RequireScope {
		// The gate tolerates a missing scope, so drop the rejected one rather
		// than invent a repository-specific replacement.
		return "", true
	}

	// An issue-style number is the only scope the harness can state as fact.
	// Any other value from the allowlist, such as `deps`, labels the pull
	// request for release and auto-merge automation that reads the scope, so an
	// underivable scope leaves the title alone instead.
	for _, number := range scopeNumbers {
		if candidate := "#" + strconv.Itoa(number); gateAllowsScope(candidate, gate) {
			return candidate, true
		}
	}
	return "", false
}

// gateAllowsScope reports whether scope satisfies the gate's scope policy. An
// empty allowlist accepts any scope, while disallowScopes always rejects.
func gateAllowsScope(scope string, gate prTitleGate) bool {
	if strings.TrimSpace(scope) == "" {
		return false
	}
	if matchesScopePattern(scope, gate.DisallowScopes) {
		return false
	}
	return len(gate.Scopes) == 0 || matchesScopePattern(scope, gate.Scopes)
}

// subjectMatchesGatePattern reports whether subject satisfies the gate's
// subjectPattern, which the action requires to match the subject end to end.
// The action evaluates JavaScript regexes, so a pattern Go cannot compile, such
// as one using a lookahead, is treated as satisfied rather than blocking every
// rewrite.
func subjectMatchesGatePattern(subject, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return true
	}
	return compiled.FindString(subject) == subject
}

func matchesScopePattern(scope string, patterns []string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
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
