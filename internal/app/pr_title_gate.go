package app

import (
	"context"
	"encoding/json"
	"errors"
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

// errPRTitleGateNotPermitted marks a title-gate repair GitHub refuses to let
// the harness perform, such as a token without pull-request write access.
// Retrying never clears it.
var errPRTitleGateNotPermitted = errors.New("pull request title edit not permitted")

// preferredPRTitleTypes orders the conventional-commit types the harness picks
// from when the current title type is rejected by the gate.
var preferredPRTitleTypes = []string{"chore", "fix", "feat", "refactor", "build", "ci", "docs", "perf", "style", "test"}

// defaultSemanticPRTitleTypes is the type list the semantic-pull-request action
// enforces when a workflow declares no `types:` input. It is the
// conventional-commit-types set the action ships with, so a gate without that
// input still rejects a type outside this list.
var defaultSemanticPRTitleTypes = []string{"build", "chore", "ci", "docs", "feat", "fix", "perf", "refactor", "revert", "style", "test"}

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
// Both the raw watch output and the summary are scanned, and neither is
// reliably the fresher source: the summary can be a snapshot taken after the
// watch output was captured, and it can equally be a snapshot kept from an
// earlier no-report retry while the watch output was refreshed. So a non-gate
// check only takes a later source's state when that state is conclusive. A
// `pending` row says a run exists, not that a failure another source reported
// cleared, and a row maxCheckSummaryChars truncation cut down to an
// unrecognized bucket says nothing at all. A gate failure seen in any source
// still counts, because the watch can be the only source that observed the
// gate run.
func prTitleGateOnlyFailure(res execx.Result, summary string) bool {
	titleGateFailed := false
	others := make(map[string]string)
	for _, raw := range []string{res.Stdout, res.Stderr, summary} {
		for name, state := range checkStatesFromText(raw) {
			if isPRTitleGateCheckName(name) {
				titleGateFailed = titleGateFailed || isFailedCheckBucket(state.Bucket)
				continue
			}
			if _, seen := others[name]; seen && !isConclusiveCheckBucket(state.Bucket) {
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
//
// A non-nil planned repair is one a snapshot veto already resolved earlier in
// this checks attempt. Reusing it spends one `gh pr view` per attempt instead
// of two; nothing edits the title in between, so the rewrite still holds.
func (h Harness) repairPRTitleGateFailure(
	ctx context.Context,
	repo repoWorkspace,
	issue *config.GitHubIssueConfig,
	checkRes execx.Result,
	checkSummary string,
	planned *prTitleGateRepair,
) (bool, error) {
	if !harnessCanRetitlePR(repo) {
		return false, nil
	}
	if !prTitleGateOnlyFailure(checkRes, checkSummary) {
		return false, nil
	}
	var plan prTitleGateRepair
	if planned != nil {
		plan = *planned
	} else {
		resolved, repairable, err := h.planPRTitleGateRepair(ctx, repo, issue)
		if err != nil || !repairable {
			return false, err
		}
		plan = resolved
	}

	if _, err := h.runPRTitleGateCommand(ctx, prEditTitleCommand(repo.Dir, repo.PRURL, plan.Title), "retitle"); err != nil {
		return false, err
	}
	h.logf(
		"stage=pr status=ok action=retitle reason=title_gate repo=%s repo_dir=%s pr_url=%s workflow=%s",
		repo.URL,
		repo.RelDir,
		repo.PRURL,
		plan.Gate.WorkflowPath,
	)
	return true, nil
}

// prTitleGateRepair is the rewrite that would clear a detected title gate.
type prTitleGateRepair struct {
	Title string
	Gate  prTitleGate
}

// planPRTitleGateRepair resolves the title the harness would set to clear the
// repository's gate. It reports false when no gate is detected, the live title
// is unavailable, or no conforming title can be derived without guessing a
// repository-specific scope.
//
// Callers use it both to perform the repair and to prove one is possible before
// a red gate is allowed to veto an otherwise green run.
func (h Harness) planPRTitleGateRepair(
	ctx context.Context,
	repo repoWorkspace,
	issue *config.GitHubIssueConfig,
) (prTitleGateRepair, bool, error) {
	gate, found := detectPRTitleGate(repo.Dir)
	if !found {
		return prTitleGateRepair{}, false, nil
	}
	current, err := h.pullRequestTitle(ctx, repo)
	if err != nil {
		return prTitleGateRepair{}, false, err
	}
	if current == "" {
		// Without the live title there is nothing to conform; a configured
		// title may belong to a different pull request on this branch.
		return prTitleGateRepair{}, false, nil
	}
	rewritten, ok := conformingPRTitle(current, gate, prTitleScopeNumbers(repo.PRURL, issue))
	if !ok {
		return prTitleGateRepair{}, false, nil
	}
	return prTitleGateRepair{Title: rewritten, Gate: gate}, true, nil
}

// runPRTitleGateCommand runs one gh command of the title-gate repair. A
// transient GitHub failure is retried, and a failure proving the harness may
// not read or edit this pull request is wrapped in errPRTitleGateNotPermitted
// so the caller can abandon the repair instead of failing a run over metadata
// it is not allowed to change.
func (h Harness) runPRTitleGateCommand(ctx context.Context, cmd execx.Command, action string) (execx.Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := h.runCommand(ctx, "pr", cmd)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return res, err
		}
		if isPRTitleGateNotPermitted(res, err) {
			return res, fmt.Errorf("%w: %v", errPRTitleGateNotPermitted, err)
		}
		reason := ""
		switch {
		case isGitHubRateLimited(res, err):
			reason = "github_rate_limited"
		case shouldRetryPRCreate(res, err):
			reason = "transient_github_error"
		}
		if reason == "" || attempt >= maxPRTitleGateCommandAttempts {
			return res, err
		}
		h.logf(
			"stage=pr status=retry action=%s reason=%s retry=%d/%d err=%q",
			action,
			reason,
			attempt,
			maxPRTitleGateCommandAttempts-1,
			err,
		)
		if sleepErr := h.Sleep(ctx, prTitleGateCommandRetryDelay); sleepErr != nil {
			return res, sleepErr
		}
	}
}

// isPRTitleGateNotPermitted reports whether a gh failure means the credentials
// in use may not read or edit the pull request. A denial releases the run
// without the repair, so only a refusal that retrying can never clear counts.
//
// GitHub reports both its primary and its secondary rate limit as HTTP 403, and
// a throttled request succeeds once the limit resets, so a rate-limited failure
// is never read as a denial.
//
// Only phrasing GitHub itself emits, in gh's own output, counts. The wrapped
// error repeats the gh arguments, which carry the pull-request title, so
// matching it would let a title that merely mentions "HTTP 403" talk the
// harness out of a repair.
func isPRTitleGateNotPermitted(res execx.Result, err error) bool {
	if err == nil {
		return false
	}
	if isGitHubRateLimited(res, err) {
		return false
	}
	if isGitHubAuthFailure(res, err) {
		return true
	}
	text := strings.ToLower(strings.Join([]string{res.Stdout, res.Stderr}, "\n"))
	return strings.Contains(text, "resource not accessible by integration") ||
		strings.Contains(text, "http 403")
}

// isGitHubRateLimited reports whether a gh failure is GitHub throttling the
// request rather than refusing it. Like the permission markers, it reads only
// gh's own output so the pull-request title cannot trigger it.
func isGitHubRateLimited(res execx.Result, err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.Join([]string{res.Stdout, res.Stderr}, "\n"))
	return strings.Contains(text, "rate limit") ||
		strings.Contains(text, "rate-limit") ||
		strings.Contains(text, "abuse detection")
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
	res, err := h.runPRTitleGateCommand(ctx, prTitleViewCommand(repo.Dir, repo.PRURL), "title_lookup")
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
			return splitWorkflowFlowSequence(lines, keyLine, keyIndent, rest)
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

// splitWorkflowFlowSequence reads an inline flow sequence, following it onto
// the lines nested under the key when the closing bracket sits on a later line.
// Leaving that bracket in place would otherwise land it on the last value.
func splitWorkflowFlowSequence(lines []string, keyLine, keyIndent int, rest string) []string {
	joined := stripWorkflowInlineComment(rest)
	if !strings.Contains(joined, "]") {
		for j := keyLine + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if lineIndent(lines[j]) <= keyIndent {
				break
			}
			item := strings.TrimSpace(stripWorkflowInlineComment(lines[j]))
			joined += " " + item
			if strings.Contains(item, "]") {
				break
			}
		}
	}
	joined = strings.TrimPrefix(strings.TrimSpace(joined), "[")
	if end := strings.LastIndex(joined, "]"); end >= 0 {
		joined = joined[:end]
	}
	return splitWorkflowScalars(strings.Split(joined, ","))
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
		item = strings.TrimSpace(stripWorkflowInlineComment(item))
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

// stripWorkflowInlineComment removes a trailing YAML comment from a line or
// flow-sequence item. A `#` only opens a comment when it follows whitespace
// outside quotes, so an issue-style scope value such as `#\d+` survives.
func stripWorkflowInlineComment(value string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if inSingle || inDouble || i == 0 {
				continue
			}
			if prev := value[i-1]; prev == ' ' || prev == '\t' {
				return value[:i]
			}
		}
	}
	return value
}

func isBlockScalarMarker(rest string) bool {
	switch strings.TrimSpace(rest) {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

func isTruthyWorkflowValue(value string) bool {
	value = stripWorkflowInlineComment(value)
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

	newType := conformingPRTitleType(currentType, gate.Types)
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

// conformingPRTitleType returns the type the gate accepts, keeping the current
// one when it is already allowed. A gate that declares no `types:` still
// enforces the action's default list, so the rejected type must be replaced
// there too rather than echoed back.
func conformingPRTitleType(current string, allowed []string) string {
	if len(allowed) == 0 {
		allowed = defaultSemanticPRTitleTypes
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, current) {
			return candidate
		}
	}
	for _, preferred := range preferredPRTitleTypes {
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, preferred) {
				return candidate
			}
		}
	}
	return allowed[0]
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
