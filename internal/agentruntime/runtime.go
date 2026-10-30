package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

const (
	HarnessCodex  = "codex"
	HarnessClaude = "claude"
)

const defaultHarness = HarnessCodex

var ErrPromptImagesUnsupported = errors.New("prompt images are unsupported for this agent harness")

var harnessDisplayNames = map[string]string{
	HarnessClaude: "Claude",
	HarnessCodex:  "Codex",
}

var promptImageHarnesses = map[string]struct{}{
	HarnessClaude: {},
	HarnessCodex:  {},
}

// RunOptions controls provider-specific execution behavior.
type RunOptions struct {
	// WorkspaceDir is the task's private parent directory, including repos and IO.
	WorkspaceDir     string
	SkipGitRepoCheck bool
	ImagePaths       []string
	WritableDirs     []string
	Env              []string
	// DisallowedTools names tools the agent CLI must not expose for this
	// invocation (for example Edit/Write/NotebookEdit for a read-only review
	// pass). Only honored by harnesses that support a tool denylist; ignored
	// otherwise.
	DisallowedTools []string
}

// Runtime describes one executable LLM harness runtime.
type Runtime struct {
	Harness    string
	Command    string
	NPMPackage string
}

type definition struct {
	defaultCommand string
	defaultPackage string
	build          func(targetDir, prompt string, opts RunOptions) (execx.Command, error)
}

var definitions = map[string]definition{
	HarnessCodex: {
		defaultCommand: HarnessCodex,
		defaultPackage: "@openai/codex@latest",
		build:          buildCodexCommand,
	},
	HarnessClaude: {
		defaultCommand: HarnessClaude,
		defaultPackage: "@anthropic-ai/claude-code@latest",
		build:          buildClaudeCommand,
	},
}

// Resolve validates harness selection and applies defaults.
func Resolve(harness, commandOverride string) (Runtime, error) {
	normalized := normalizeHarness(harness)
	def, ok := definitions[normalized]
	if !ok {
		return Runtime{}, fmt.Errorf(
			"unsupported agentHarness %q; supported values: %s",
			strings.TrimSpace(harness),
			strings.Join(SupportedHarnesses(), ", "),
		)
	}

	command := strings.TrimSpace(commandOverride)
	if command == "" {
		command = def.defaultCommand
	}

	return Runtime{
		Harness:    normalized,
		Command:    command,
		NPMPackage: def.defaultPackage,
	}, nil
}

// Default returns the default runtime selection.
func Default() Runtime {
	def := definitions[defaultHarness]
	return Runtime{
		Harness:    defaultHarness,
		Command:    def.defaultCommand,
		NPMPackage: def.defaultPackage,
	}
}

// SupportedHarnesses returns supported harness names in stable order.
func SupportedHarnesses() []string {
	keys := make([]string, 0, len(definitions))
	for key := range definitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// DisplayName returns the user-facing harness label.
func DisplayName(harness string) string {
	normalized := normalizeHarness(harness)
	if label, ok := harnessDisplayNames[normalized]; ok {
		return label
	}
	return harnessDisplayNames[defaultHarness]
}

// SupportsPromptImages reports whether the selected harness accepts prompt image attachments.
func SupportsPromptImages(harness string) bool {
	_, ok := promptImageHarnesses[normalizeHarness(harness)]
	return ok
}

// SupportedPromptImageHarnesses returns the harnesses that accept prompt images.
func SupportedPromptImageHarnesses() []string {
	out := make([]string, 0, len(promptImageHarnesses))
	for harness := range promptImageHarnesses {
		out = append(out, harness)
	}
	sort.Strings(out)
	return out
}

// UnsupportedPromptImagesError returns a stable error for unsupported image attachments.
func UnsupportedPromptImagesError(harness string) error {
	label := strings.TrimSpace(DisplayName(harness))
	if label == "" {
		label = DisplayName(defaultHarness)
	}
	supported := supportedPromptImageHarnessLabels()
	if supported == "" {
		return fmt.Errorf("%s does not support prompt images: %w", label, ErrPromptImagesUnsupported)
	}
	return fmt.Errorf(
		"%s does not support prompt images. Remove screenshots or switch to %s: %w",
		label,
		supported,
		ErrPromptImagesUnsupported,
	)
}

// RequirementName returns the boot diagnostic requirement key for this runtime.
func (r Runtime) RequirementName() string {
	return normalizeHarness(r.Harness) + "_cli"
}

// PreflightCommand returns the command used to verify CLI availability.
func (r Runtime) PreflightCommand() execx.Command {
	return execx.Command{Name: strings.TrimSpace(r.Command), Args: []string{"--help"}}
}

// BuildCommand builds an execution command for this runtime.
func (r Runtime) BuildCommand(targetDir, prompt string, opts RunOptions) (execx.Command, error) {
	if root := strings.TrimSpace(opts.WorkspaceDir); root != "" {
		absolute, err := resolveSandboxPath(root)
		if err != nil {
			return execx.Command{}, fmt.Errorf("resolve agent workspace: %w", err)
		}
		opts.WorkspaceDir = absolute
		if strings.TrimSpace(targetDir) == "" {
			return execx.Command{}, fmt.Errorf("agent working directory is required for task sandbox")
		}
		targetDir, err = resolveSandboxPath(targetDir)
		if err != nil {
			return execx.Command{}, fmt.Errorf("resolve agent working directory: %w", err)
		}
		// Resolve paths once, before the child changes cwd. Otherwise a relative
		// directory could validate here but grant a different root in the CLI.
		dirs := make([]string, 0, len(opts.WritableDirs))
		for _, dir := range opts.WritableDirs {
			if dir = strings.TrimSpace(dir); dir != "" {
				resolved, err := resolveSandboxPath(dir)
				if err != nil {
					return execx.Command{}, fmt.Errorf("resolve writable agent directory: %w", err)
				}
				dirs = append(dirs, resolved)
			}
		}
		opts.WritableDirs = dirs
	}
	if err := validateWorkspaceDirs(targetDir, opts); err != nil {
		return execx.Command{}, err
	}
	def, ok := definitions[normalizeHarness(r.Harness)]
	if !ok {
		return execx.Command{}, fmt.Errorf("unsupported runtime harness %q", r.Harness)
	}

	cmd, err := def.build(targetDir, prompt, opts)
	if err != nil {
		return execx.Command{}, err
	}
	cmd.Name = strings.TrimSpace(r.Command)
	if cmd.Name == "" {
		return execx.Command{}, fmt.Errorf("runtime command is required")
	}
	if normalizeHarness(r.Harness) == HarnessClaude {
		// Sandbox Claude's whole process, including its provider requests.
		// Reviews need network access with a genuinely read-only filesystem:
		// workspace-write implicitly permits cwd even with writable_roots=[].
		readOnly := len(nonEmptyStrings(opts.DisallowedTools)) > 0
		configDir, createConfigDir, err := claudeSandboxConfigRoot(opts.Env, opts.WorkspaceDir)
		if err != nil {
			return execx.Command{}, err
		}
		args := []string{"sandbox"}
		if readOnly {
			filesystem := `":root"="read"`
			if opts.WorkspaceDir != "" {
				// Claude needs its isolated cache, temp files and session state.
				// Permit only harness-owned IO, never the repository or task root.
				ioDir := filepath.Join(opts.WorkspaceDir, ".agent00-agent-io")
				resolved, err := resolveSandboxPath(ioDir)
				if err != nil || resolved != ioDir {
					return execx.Command{}, fmt.Errorf("review agent IO directory must not be a symlink")
				}
				filesystem += fmt.Sprintf(",%q=\"write\"", ioDir)
			}
			if configDir != "" {
				// A review run writes session state and rotates credentials in
				// CLAUDE_CONFIG_DIR exactly like an implementation run, and it
				// fails with the same silent EROFS when that directory is
				// read-only. The grant covers only that one directory, so the
				// repository and the task root stay read-only as the review
				// contract requires.
				filesystem += fmt.Sprintf(",%q=\"write\"", configDir)
			}
			args = append(args,
				"-P", "agent00_review",
				"-c", "permissions.agent00_review={filesystem={"+filesystem+"},network={enabled=true}}",
			)
		} else {
			sandboxOpts := opts
			if configDir != "" {
				sandboxOpts.WritableDirs = append(append([]string(nil), opts.WritableDirs...), configDir)
			}
			args = append(args, "-c", `sandbox_mode="workspace-write"`)
			args = append(args, workspaceSandboxArgs(sandboxOpts)...)
		}
		args = append(args, "--", cmd.Name)
		cmd.Args = append(args, cmd.Args...)
		cmd.Name = "codex"
		if opts.WorkspaceDir != "" {
			// The sandbox helper needs no login or operator configuration.
			env := cmd.Env
			if len(env) == 0 {
				env = os.Environ()
			}
			cmd.Env = make([]string, 0, len(env)+1)
			for _, entry := range env {
				if !strings.HasPrefix(entry, "CODEX_HOME=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "CODEX_HOME="+filepath.Join(opts.WorkspaceDir, ".agent00-agent-io", "config", "sandbox"))
		}
		if createConfigDir {
			// The one filesystem effect of building a command, kept last so a
			// command that fails to build leaves nothing behind. It creates the
			// caller's configured directory and nothing else: a missing parent
			// is a misconfigured path, not a directory tree to materialize.
			// Owner-only, because rotated credentials land in it.
			if err := os.Mkdir(configDir, 0o700); err != nil {
				if !os.IsExist(err) {
					return execx.Command{}, fmt.Errorf("create CLAUDE_CONFIG_DIR %q: %w", configDir, err)
				}
				// Lost the race with a concurrent run; the grant still has to
				// point at a directory.
				if info, statErr := os.Stat(configDir); statErr != nil || !info.IsDir() {
					return execx.Command{}, fmt.Errorf("CLAUDE_CONFIG_DIR %q is not a directory", configDir)
				}
			}
		}
	}
	return cmd, nil
}

// claudeSandboxConfigRoot returns the one directory outside the task workspace
// that an implementation run must still be able to write: the caller-supplied
// CLAUDE_CONFIG_DIR.
//
// Claude writes there on every Bash tool call - its `session-env` directory,
// project state, and the rotated credentials that the harness deliberately
// keeps persistent and shared across tasks instead of copying per run. With
// the task workspace as the only writable root, those writes fail with
// "EROFS: read-only file system". The CLI itself keeps running, so the failure
// is silent and total: the agent loses the shell for the whole task - no
// tests, linters, git or gh - and a CI remediation round can only guess at
// what broke. Credential refresh fails the same way.
//
// Only an explicit CLAUDE_CONFIG_DIR in the invocation's own environment is
// honored. The grant never comes from the harness process environment: a
// writable root must be a deliberate decision by the caller that prepared the
// agent environment, never an artifact of whoever happened to launch the
// harness. (validateSandboxConfigRoot does read the process home directory,
// but only to refuse an overly broad grant, never to create one.) Review runs
// get the same grant, with everything else still read-only.
//
// The second return value reports that the directory is missing and has to be
// created before the command runs. Nothing else creates it - the caller only
// exports the path, and the CLI first writes there from inside the sandbox,
// where a missing root is merely read-only, so an ungranted first run leaves
// the agent with the same dead shell as no grant at all. BuildCommand creates
// it once everything else has validated.
func claudeSandboxConfigRoot(environ []string, workspaceDir string) (string, bool, error) {
	if len(environ) == 0 {
		return "", false, nil
	}
	configDir := ""
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok && key == "CLAUDE_CONFIG_DIR" {
			configDir = strings.TrimSpace(value)
		}
	}
	if configDir == "" {
		return "", false, nil
	}
	resolved, err := resolveSandboxPath(configDir)
	if err != nil {
		// A CLAUDE_CONFIG_DIR that cannot be resolved is a misconfigured
		// environment, not an absent grant. Swallowing it into "no writable
		// root" reproduces the silent EROFS this function exists to prevent,
		// so the caller fails the run with the reason instead.
		return "", false, fmt.Errorf("resolve CLAUDE_CONFIG_DIR %q: %w", configDir, err)
	}
	if err := validateSandboxConfigRoot(resolved, workspaceDir); err != nil {
		return "", false, err
	}
	info, statErr := os.Stat(resolved)
	switch {
	case statErr == nil && !info.IsDir():
		return "", false, fmt.Errorf("CLAUDE_CONFIG_DIR %q is not a directory", resolved)
	case os.IsNotExist(statErr):
		return resolved, true, nil
	case statErr != nil:
		return "", false, fmt.Errorf("stat CLAUDE_CONFIG_DIR %q: %w", resolved, statErr)
	}
	return resolved, false, nil
}

// validateSandboxConfigRoot bounds the one writable root that deliberately sits
// outside the task workspace. validateWorkspaceDirs cannot vet it - it rejects
// exactly the directories that escape the workspace - so the breadth checks
// live here. A filesystem root, a shared top-level directory, a home directory
// or any of its ancestors, or any ancestor of the task workspace would quietly
// hand the agent write access to operator state and to other tasks' worktrees,
// which is the opposite of what this grant exists for.
//
// The home-directory check reads the process home (os.UserHomeDir, so $HOME)
// on purpose: it is the operator state this guard protects. That is the one
// place the process environment is consulted, and it can only narrow the
// grant - a caller's CLAUDE_CONFIG_DIR is still the only thing that can widen
// it, and an unset or unreadable home just skips this check.
func validateSandboxConfigRoot(resolved, workspaceDir string) error {
	if resolved == filepath.Dir(resolved) {
		return fmt.Errorf("CLAUDE_CONFIG_DIR %q is a filesystem root and would widen the agent sandbox", resolved)
	}
	// A directory one level below the filesystem root - /home, /mnt, /tmp,
	// /Users - is a shared parent: other operators' home directories and
	// other tasks' workspaces live directly under it, so it is never a
	// configuration directory and always a far wider grant than intended.
	if sandboxPathDepth(resolved) < 2 {
		return fmt.Errorf("CLAUDE_CONFIG_DIR %q is a shared top-level directory and would widen the agent sandbox", resolved)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		if resolvedHome, err := resolveSandboxPath(home); err == nil {
			if resolvedHome == resolved {
				return fmt.Errorf("CLAUDE_CONFIG_DIR %q is the home directory and would widen the agent sandbox", resolved)
			}
			if sandboxPathContains(resolved, resolvedHome) {
				return fmt.Errorf(
					"CLAUDE_CONFIG_DIR %q contains the home directory %q and would widen the agent sandbox",
					resolved,
					resolvedHome,
				)
			}
		}
	}
	if strings.TrimSpace(workspaceDir) == "" {
		return nil
	}
	workspace, err := filepath.Abs(workspaceDir)
	if err != nil {
		return fmt.Errorf("resolve agent workspace: %w", err)
	}
	if sandboxPathContains(resolved, workspace) {
		return fmt.Errorf(
			"CLAUDE_CONFIG_DIR %q contains the task workspace %q and would widen the agent sandbox",
			resolved,
			workspace,
		)
	}
	return nil
}

// sandboxPathContains reports whether parent is child or one of its ancestors.
func sandboxPathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// sandboxPathDepth counts the path elements below the filesystem root, so
// "/mnt" is 1 and "/home/agent/.claude" is 3.
func sandboxPathDepth(path string) int {
	depth := 0
	for {
		parent := filepath.Dir(path)
		if parent == path {
			return depth
		}
		if base := filepath.Base(path); base != "." && base != string(filepath.Separator) {
			depth++
		}
		path = parent
	}
}

// Resolve existing symlinks even when a descendant will be created later.
// Never treat a dangling symlink as a safe, not-yet-created directory.
func resolveSandboxPath(dir string) (string, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("sandbox directory %q is a dangling symlink", path)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveSandboxPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func workspaceSandboxArgs(opts RunOptions) []string {
	roots := make([]string, 0, len(opts.WritableDirs)+1)
	if root := strings.TrimSpace(opts.WorkspaceDir); root != "" {
		roots = append(roots, root)
	}
	for _, dir := range opts.WritableDirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			roots = append(roots, dir)
		}
	}
	unique := roots[:0]
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if !seen[root] {
			seen[root] = true
			unique = append(unique, root)
		}
	}
	encoded, _ := json.Marshal(unique)
	return []string{
		"-c", "sandbox_workspace_write.writable_roots=" + string(encoded),
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
		// Both runtimes need network access for dependency downloads and tests.
		"-c", "sandbox_workspace_write.network_access=true",
	}
}

func validateWorkspaceDirs(targetDir string, opts RunOptions) error {
	if strings.TrimSpace(opts.WorkspaceDir) == "" {
		return nil
	}
	root, err := filepath.Abs(opts.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("resolve agent workspace: %w", err)
	}
	for _, dir := range append([]string{targetDir}, opts.WritableDirs...) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		path, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("resolve agent directory: %w", err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("agent directory %q is outside task workspace %q", dir, root)
		}
	}
	return nil
}

func normalizeHarness(harness string) string {
	normalized := strings.ToLower(strings.TrimSpace(harness))
	if normalized == "" {
		return defaultHarness
	}
	return normalized
}

func buildCodexCommand(targetDir, prompt string, opts RunOptions) (execx.Command, error) {
	// Codex has no per-tool denylist like Claude's --disallowedTools. When the
	// caller asks for a read-only pass (for example a post-task review that
	// must not touch the repository), the only enforceable equivalent is to
	// drop the whole invocation into Codex's read-only sandbox instead of
	// workspace-write, so the CLI cannot write anywhere regardless of which
	// tool it tries to use.
	readOnly := len(nonEmptyStrings(opts.DisallowedTools)) > 0
	sandboxMode := "workspace-write"
	if readOnly {
		sandboxMode = "read-only"
	}
	args := []string{"exec", "--sandbox", sandboxMode, "-c", `approval_policy="never"`}
	if !readOnly {
		args = append(args, workspaceSandboxArgs(opts)...)
	}
	if opts.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if !readOnly {
		for _, dir := range opts.WritableDirs {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			args = append(args, "--add-dir", dir)
		}
	}
	for _, imagePath := range opts.ImagePaths {
		imagePath = strings.TrimSpace(imagePath)
		if imagePath == "" {
			continue
		}
		args = append(args, "--image", imagePath)
	}

	return execx.Command{
		Dir:   targetDir,
		Args:  args,
		Env:   append([]string(nil), opts.Env...),
		Stdin: prompt,
	}, nil
}

func buildClaudeCommand(targetDir, prompt string, opts RunOptions) (execx.Command, error) {
	args := []string{
		"--print",
		"--output-format", "text",
		"--dangerously-skip-permissions",
	}
	for _, dir := range opts.WritableDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		args = append(args, "--add-dir", dir)
	}
	if tools := nonEmptyStrings(opts.DisallowedTools); len(tools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, tools...)
	}
	// Keep task content out of argv: subprocess errors include the arguments,
	// and large review packets can exceed the OS per-argument size limit.
	args = append(args, "--")
	return execx.Command{Dir: targetDir, Args: args, Env: append([]string(nil), opts.Env...), Stdin: prompt}, nil
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func countNonEmptyStrings(values []string) int {
	count := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func validatePromptImageSupport(harness string, imagePaths []string) error {
	if countNonEmptyStrings(imagePaths) == 0 {
		return nil
	}
	if SupportsPromptImages(harness) {
		return nil
	}
	return UnsupportedPromptImagesError(harness)
}

func supportedPromptImageHarnessLabels() string {
	labels := make([]string, 0, len(promptImageHarnesses))
	seen := make(map[string]struct{}, len(promptImageHarnesses))
	for _, harness := range SupportedPromptImageHarnesses() {
		label := strings.TrimSpace(DisplayName(harness))
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " or " + labels[1]
	default:
		return strings.Join(labels[:len(labels)-1], ", ") + ", or " + labels[len(labels)-1]
	}
}
