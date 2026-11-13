package failurefollowup

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/config"
)

const RequiredPrompt = "Review the failing log paths first, identify every root cause behind the failed task, fix the underlying agent_00 application issues in this repository, validate locally where possible, and summarize the verified results. The failed run's original task prompt is withheld and is not supplied here; do not reconstruct it, ask for it, or implement the product change it requested unless that change is required to fix agent_00 failure handling."

// RedactedTaskPromptNotice stands in for the failed run's task prompt inside
// follow-up context blocks. Follow-up runs execute against the agent_00
// repository, so prompt text that can name a customer's repositories, people,
// or source code must never be copied into them. The failing logs the notice
// points at can still carry those details, so it also bars copying them into
// anything this run leaves behind.
const RedactedTaskPromptNotice = "Task prompt: redacted by the harness. Follow-up runs execute against the agent_00 repository, so the failed run's prompt text is withheld because it can contain private repository, personnel, or source details. Diagnose from the failure context and the failing log paths above. Those logs can contain the same private details: use them only for diagnosis, and never copy customer repository names, URLs, branch names, file paths, personnel, or source snippets from them into agent_00 code, tests, docs, commit messages, pull request titles or bodies, or comments."

const FollowUpRepositoryURL = config.DefaultRepositoryURL

const FailureResponseInstruction = "When failures occur, send a response back to the calling agent that clearly states failure and includes the error details. Use explicit `Failure:` and `Error details:` fields."
const RuntimeToolingInstruction = "Runtime tooling available to agents includes Playwright for local browser testing, screenshots, and comparisons; npm for JavaScript package installs, scripts, tests, and builds; Python with pip and virtualenv; and Go tooling such as `go test` and `go build`. Use the matching local tooling when it fits the repository."
const ValidationToolingInstruction = "If local test or validation tooling is unavailable in this runtime (for example `command not found`, missing `node_modules`, a missing repository validation script or task, or restricted network access blocking dependency downloads), do not fail solely for that and do not use `Failure:` solely for that validation gap. Continue with repository changes, run any alternative checks you can, and clearly report the validation gap."
const ActivityPrivacyInstruction = "Before sharing repository or pull-request links in public activity, use `gh repo view OWNER/REPO --json isPrivate,nameWithOwner` during clone or PR tooling. Share repo and PR links only when GitHub reports `isPrivate:false`; never share private repository links."
const UninitializedRepoInstruction = "If a repository is not initialized after clone, use only gh CLI/git tools to create and push a main branch, then continue once git state is ready for work."
const RemoteOperationsInstruction = "Do not commit, push, create or update pull requests, or watch remote CI/CD from inside this agent runtime. Repository instructions may define when those lifecycle steps are allowed or what they must contain, but the harness performs them after your local work completes. Finish the repository changes and local validation you can perform, then leave the worktree changes for the harness."
const ActionableChangeInstruction = "For implementation or repository-change requests, do not stop at analysis. Produce the smallest correct repository diff that satisfies the request unless you can cite concrete file evidence that the requested outcome already exists."
const NoOpInstruction = "Only return a no-op when the task is genuinely review/investigation-only or you can cite concrete repository evidence that no file changes are required. Do not force an empty PR."

var ExecutionContract = strings.Join([]string{
	FailureResponseInstruction,
	RuntimeToolingInstruction,
	ValidationToolingInstruction,
	ActivityPrivacyInstruction,
	UninitializedRepoInstruction,
	RemoteOperationsInstruction,
	ActionableChangeInstruction,
	NoOpInstruction,
}, "\n\n")

var FollowUpContract = strings.Join([]string{
	ExecutionContract,
	fmt.Sprintf(`Harness-managed follow-up targeting:
{"repos":["%s"],"targetSubdir":".","prompt":"%s"}`, FollowUpRepositoryURL, RequiredPrompt),
}, "\n\n")

const (
	LogFileName           = "terminal.log"
	LegacyTaskLogFileName = "term"
	FallbackLogSubdir     = "main"
)

var nonRemediableRepoAccessMarkers = []string{
	"write access to repository not granted",
	"requested url returned error: 403",
	".git denied to",
	"authentication failed",
	"could not read username for 'https://github.com'",
	"doesn't have the rights to pull the code",
	"doesn't have the rights to push a pr",
	"refusing to allow an oauth app to create or update workflow",
	"without workflow scope",
	"without `workflow` scope",
}

const githubSSHPermissionDeniedReason = "permission to repository denied"

const unreachableCoverageTargetReason = "coverage target cannot be met safely"
const missingSecurityContractReason = "security verification contract is missing"

var nonRemediableFailureMarkers = []string{
	"no space left on device",
	"no permissions to create a new namespace",
	"sandbox namespace creation denied",
	"workspace storage",
	"quota exceeded",
	"you've hit your session limit",
	"you’ve hit your session limit",
	"insufficient_quota",
	"billing",
	"organization has disabled claude subscription access",
	"401 unauthorized",
	"missing bearer or basic authentication",
	"invalid api key",
	"invalid_authentication",
	"authentication error",
	"sandbox rejected writes to",
	"writing outside of the project; rejected by user approval settings",
	"shell network blocked",
	"could not resolve host",
	"temporary failure in name resolution",
	"no such host",
	"browser tool crawled public pages, but cannot save binary assets",
	"would require invented",
	"requires invented",
	"require fabricating",
	"task premise false",
	"can't fabricate",
	"cannot fabricate",
	"source outside active scope",
}

func WithExecutionContract(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ExecutionContract
	}
	if strings.Contains(base, ExecutionContract) {
		return base
	}
	return base + "\n\n" + ExecutionContract
}

func WithFollowUpContract(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return FollowUpContract
	}
	if strings.Contains(base, FollowUpContract) {
		return base
	}
	if !strings.Contains(base, fmt.Sprintf(`{"repos":["%s"],"targetSubdir":".","prompt":"%s"}`, FollowUpRepositoryURL, RequiredPrompt)) {
		base += "\n\n" + fmt.Sprintf(`Harness-managed follow-up targeting:
{"repos":["%s"],"targetSubdir":".","prompt":"%s"}`, FollowUpRepositoryURL, RequiredPrompt)
	}
	return WithExecutionContract(base)
}

// RedactTaskPrompt returns the notice that replaces a failed run's task prompt
// in follow-up context, or an empty string when that run carried no prompt.
func RedactTaskPrompt(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return ""
	}
	return RedactedTaskPromptNotice
}

// Placeholders substituted for customer-derived identifiers by
// RedactFailureDetail.
const (
	RedactedURLPlaceholder    = "[redacted-url]"
	RedactedPathPlaceholder   = "[redacted-path]"
	RedactedBranchPlaceholder = "[redacted-branch]"
)

var (
	// Clone, remote, and pull request links.
	schemeURLPattern = regexp.MustCompile(`(?i)\b(?:https?|ssh|git|file)://[^\s"'<>]+`)

	// SCP-style git remotes such as git@github.com:acme/repo.git, including the
	// owner-less git@host:repo.git form.
	scpRemotePattern = regexp.MustCompile(`[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/-]+`)

	// Workspace, clone, and log directories. The leading group keeps relative
	// agent_00 paths such as cmd/harness/main.go intact.
	absolutePathPattern = regexp.MustCompile(`(\A|[^A-Za-z0-9._@%+-])((?:/[A-Za-z0-9._@%+-]+){2,}/?)`)

	// Bare owner/repo references left by git errors such as
	// "Permission to acme/repo.git denied to someone".
	repoReferencePattern = regexp.MustCompile(`(?i)\b[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\.git\b`)

	// Prompt-derived branch slugs; see slug.BranchName.
	branchSlugPattern = regexp.MustCompile(`(?i)\bagent00-[A-Za-z0-9._-]+`)
)

// RedactFailureDetail strips customer-derived identifiers out of a failed run's
// error text so a follow-up that executes against agent_00 can quote it.
// Repository URLs, pull request URLs, absolute filesystem paths, and
// prompt-derived branch slugs all name the customer's repository or task, so
// each is replaced with a placeholder while the surrounding diagnostic wording
// is preserved.
func RedactFailureDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	detail = schemeURLPattern.ReplaceAllString(detail, RedactedURLPlaceholder)
	detail = scpRemotePattern.ReplaceAllString(detail, RedactedURLPlaceholder)
	detail = absolutePathPattern.ReplaceAllString(detail, "${1}"+RedactedPathPlaceholder)
	detail = repoReferencePattern.ReplaceAllString(detail, RedactedURLPlaceholder)
	detail = branchSlugPattern.ReplaceAllString(detail, RedactedBranchPlaceholder)
	return strings.TrimSpace(detail)
}

func ComposePrompt(requiredPrompt string, logPaths, fallbackLogPaths []string, noPathGuidance, contextBlock string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(requiredPrompt))
	b.WriteString("\n\nRelevant failing log path(s):")

	appendPaths := func(paths []string) bool {
		wrote := false
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			b.WriteString("\n- ")
			b.WriteString(path)
			wrote = true
		}
		return wrote
	}

	if !appendPaths(logPaths) && !appendPaths(fallbackLogPaths) && strings.TrimSpace(noPathGuidance) != "" {
		b.WriteString("\n- ")
		b.WriteString(strings.TrimSpace(noPathGuidance))
	}

	contextBlock = strings.TrimSpace(contextBlock)
	if contextBlock != "" {
		b.WriteString("\n\n")
		b.WriteString(contextBlock)
	}

	return WithFollowUpContract(b.String())
}

func FollowUpTargeting(baseBranch, targetSubdir, currentBranch string) (string, string) {
	return "", "."
}

func TaskLogPaths(logRoot, requestID string) []string {
	logRoot = strings.TrimSpace(logRoot)
	logDir, ok := TaskLogDir(logRoot, requestID)
	if !ok {
		return nil
	}

	paths := make([]string, 0, 5)
	seen := make(map[string]struct{}, 5)
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	appendTaskLogPaths := func(taskDir string, includeDir bool) {
		if includeDir {
			appendPath(taskDir)
		}
		appendPath(filepath.Join(taskDir, LegacyTaskLogFileName))
		appendPath(filepath.Join(taskDir, LogFileName))
	}

	appendTaskLogPaths(logDir, true)
	if shouldIncludeFallbackTaskLogs(requestID) {
		appendTaskLogPaths(filepath.Join(logRoot, FallbackLogSubdir), false)
	}

	return paths
}

func shouldIncludeFallbackTaskLogs(requestID string) bool {
	subdir, ok := IdentifierSubdir(requestID)
	if !ok {
		return true
	}
	subdir = filepath.Clean(subdir)
	localPrefix := "local" + string(filepath.Separator)
	return !(subdir == "local" || strings.HasPrefix(subdir, localPrefix))
}

func TaskLogDir(logRoot, requestID string) (string, bool) {
	logRoot = strings.TrimSpace(logRoot)
	requestID = strings.TrimSpace(requestID)
	if logRoot == "" || requestID == "" {
		return "", false
	}

	subdir, ok := IdentifierSubdir(requestID)
	if !ok {
		return "", false
	}
	subdir = filepath.Clean(subdir)
	if subdir == "." || subdir == "" || subdir == ".." {
		return "", false
	}
	if filepath.IsAbs(subdir) || strings.HasPrefix(subdir, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(logRoot, subdir), true
}

func IdentifierSubdir(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}

	rawParts := strings.Split(id, "-")
	parts := make([]string, 0, len(rawParts))
	for _, rawPart := range rawParts {
		part := SanitizeLogPathPart(rawPart)
		if part == "" {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return FallbackLogSubdir, true
	}
	return filepath.Join(parts...), true
}

func SanitizeLogPathPart(part string) string {
	part = strings.TrimSpace(part)
	if part == "" {
		return ""
	}

	var b strings.Builder
	lastSeparator := false
	for i := 0; i < len(part); i++ {
		ch := part[i]
		switch {
		case (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'):
			b.WriteByte(ch)
			lastSeparator = false
		case ch == '.' || ch == '_' || ch == '-':
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte(ch)
			lastSeparator = false
		default:
			if b.Len() == 0 || lastSeparator {
				continue
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}

	trimmed := strings.Trim(b.String(), ".-_")
	if trimmed == "" {
		return ""
	}
	return trimmed
}

func NonRemediableRepoAccessReason(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return ""
	}
	for _, marker := range nonRemediableRepoAccessMarkers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	if strings.Contains(text, "permission to ") &&
		strings.Contains(text, " denied to ") &&
		strings.Contains(text, "could not read from remote repository") {
		return githubSSHPermissionDeniedReason
	}
	return ""
}

func NonRemediableFailureReason(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return ""
	}
	if agentruntime.CodexAuthenticationFailure(text) {
		return "codex authentication required"
	}
	if isUnreachableCoverageTargetFailure(text) {
		return unreachableCoverageTargetReason
	}
	if isMissingSecurityVerificationContractFailure(text) {
		return missingSecurityContractReason
	}
	for _, marker := range nonRemediableFailureMarkers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return NonRemediableRepoAccessReason(err)
}

// isMissingSecurityVerificationContractFailure recognizes a safe refusal to
// invent webhook authentication when the customer repository supplies no
// verification contract. A follow-up against agent_00 cannot discover the
// provider-specific secret or signature scheme needed to implement that task.
// Keep the match compound so ordinary, actionable webhook bugs still receive
// a failure follow-up.
func isMissingSecurityVerificationContractFailure(text string) bool {
	if !strings.Contains(text, "webhook") {
		return false
	}

	hasVerificationContext := containsAny(text, []string{
		"authentication",
		"verification",
		"verifier",
		"signature",
		"signing-secret",
		"signing secret",
	})
	hasMissingContract := containsAny(text, []string{
		"no verification contract",
		"missing verification contract",
		"no signature contract",
		"missing signature contract",
		"no verifier or signing-secret contract",
		"no verifier or signing secret contract",
		"no signing-secret contract",
		"no signing secret contract",
		"missing signing-secret contract",
		"missing signing secret contract",
	})
	hasUnsafeOutcome := containsAny(text, []string{
		"unresolved safely",
		"cannot safely",
		"can't safely",
		"would break provider",
		"fake security",
	})
	return hasVerificationContext && hasMissingContract && hasUnsafeOutcome
}

func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// isUnreachableCoverageTargetFailure recognizes a measured customer-repository
// coverage shortfall. A follow-up runs against agent_00, so it cannot add the
// customer tests needed to satisfy this target.
func isUnreachableCoverageTargetFailure(text string) bool {
	if strings.Contains(text, "cannot safely meet coverage target") {
		return true
	}
	return strings.Contains(text, "covered lines") &&
		(strings.Contains(text, "target not met") || strings.Contains(text, "would fail ci"))
}
