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
		// Claude creates per-session scratch state next to its persistent
		// configuration the first time a task uses a tool. That directory sits
		// outside the task workspace on purpose, so without this grant the CLI
		// hits a read-only filesystem on its first tool call and runs the rest
		// of the task without it.
		scratchDir, scratchErr := claudeConfigSandboxRoot(claudeSandboxEnv(opts))
		if scratchErr != nil {
			return execx.Command{}, fmt.Errorf("prepare Claude sandbox scratch directory: %w", scratchErr)
		}
		// Sandbox Claude's whole process, including its provider requests.
		// Reviews need network access with a genuinely read-only filesystem:
		// workspace-write implicitly permits cwd even with writable_roots=[].
		readOnly := len(nonEmptyStrings(opts.DisallowedTools)) > 0
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
			if scratchDir != "" {
				// A review boots the same CLI tool runtime a task run does, so
				// it fails on the same unwritable scratch directory without this.
				filesystem += fmt.Sprintf(",%q=\"write\"", scratchDir)
			}
			args = append(args,
				"-P", "agent00_review",
				"-c", "permissions.agent00_review={filesystem={"+filesystem+"},network={enabled=true}}",
			)
		} else {
			args = append(args, "-c", `sandbox_mode="workspace-write"`)
			args = append(args, workspaceSandboxArgs(opts, scratchDir)...)
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
	}
	return cmd, nil
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

// workspaceSandboxArgs builds the sandbox restrictions for one invocation.
// extraRoots carries harness-owned directories outside the task workspace that
// the CLI itself must keep writing to; they bypass validateWorkspaceDirs
// because they never come from task configuration.
func workspaceSandboxArgs(opts RunOptions, extraRoots ...string) []string {
	roots := make([]string, 0, len(opts.WritableDirs)+len(extraRoots)+1)
	if root := strings.TrimSpace(opts.WorkspaceDir); root != "" {
		roots = append(roots, root)
	}
	for _, dir := range opts.WritableDirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			roots = append(roots, dir)
		}
	}
	for _, dir := range extraRoots {
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

// claudeConfigSandboxRoot resolves the one directory inside the persistent
// Claude configuration directory the CLI must keep writing to from inside the
// sandbox. Granting that directory instead of the whole configuration
// directory keeps the stored credentials and Claude's own configuration
// backups out of reach of model-issued commands.
//
// It returns an empty string when there is nothing to grant: when the
// invocation environment does not describe a configuration directory, because
// the CLI cannot resolve one the environment omits either, and when the
// configuration directory cannot be extended, because the CLI cannot write
// there either. Every other failure is returned instead of dropped, because a
// silently missing grant puts the CLI back on a read-only filesystem and the
// task then finishes without touching a file.
func claudeConfigSandboxRoot(env []string) (string, error) {
	if len(env) == 0 || !describesClaudeConfigDir(env) {
		return "", nil
	}
	home, err := ClaudeHome(env)
	if err != nil {
		return "", err
	}
	scratch, err := claudeSandboxScratchDir(home)
	if err != nil || strings.TrimSpace(scratch) == "" {
		return "", err
	}
	return resolveSandboxPath(scratch)
}

// claudeSandboxEnv returns the environment the CLI actually resolves its
// configuration directory from. buildClaudeCommand passes opts.Env through
// verbatim, so an invocation that carries no environment inherits the harness
// process environment instead. Reading opts.Env alone in that case drops the
// grant while the CLI still resolves a real configuration directory, which puts
// it back on a read-only filesystem for its first tool call.
//
// An invocation without a task workspace is not sandboxed into one, so it has
// no scratch grant to make and keeps reading opts.Env as given.
func claudeSandboxEnv(opts RunOptions) []string {
	if len(opts.Env) > 0 || strings.TrimSpace(opts.WorkspaceDir) == "" {
		return opts.Env
	}
	return os.Environ()
}

// describesClaudeConfigDir reports whether env names a persistent Claude
// configuration directory at all, mirroring what ClaudeHome reads.
func describesClaudeConfigDir(env []string) bool {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if key == "CLAUDE_CONFIG_DIR" || key == "HOME" {
			return true
		}
	}
	return false
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
		// Every harness invocation is a one-shot task. Do not spend model work
		// generating a suggested next prompt that the harness cannot consume.
		"--prompt-suggestions", "false",
		// The harness never resumes a Claude session, so avoid persisting session
		// state after each task.
		"--no-session-persistence",
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
