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
			args = append(args,
				"-P", "agent00_review",
				"-c", "permissions.agent00_review={filesystem={"+filesystem+"},network={enabled=true}}",
			)
		} else {
			args = append(args, "-c", `sandbox_mode="workspace-write"`)
			// Claude's whole process runs inside this sandbox, so its own
			// configuration directory has to stay writable. The CLI creates
			// per-session scratch state under $CLAUDE_CONFIG_DIR (for example
			// session-env) before the Bash tool can run, and it rotates its
			// refresh token in the same place. That directory deliberately
			// points at the persistent operator config instead of a task-local
			// copy, so it sits outside the task workspace: leaving it read-only
			// fails every shell command with EROFS and silently costs the agent
			// its local lint/test/build validation.
			configDir, err := claudeConfigDirFromEnv(opts.Env)
			if err != nil {
				return execx.Command{}, err
			}
			if configDir != "" {
				opts.WritableDirs = append(opts.WritableDirs, configDir)
			}
			args = append(args, workspaceSandboxArgs(opts)...)
			args = append(args, "-c", "sandbox_workspace_write.network_access=true")
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

// claudeConfigDirFromEnv resolves CLAUDE_CONFIG_DIR from the invocation
// environment. Only the explicit variable is honored: the harness always sets
// it, and falling back to the ambient process environment would grant a
// directory the caller never selected write access inside the sandbox.
func claudeConfigDirFromEnv(environ []string) (string, error) {
	value := ""
	for _, entry := range environ {
		if name, entryValue, ok := strings.Cut(entry, "="); ok && name == "CLAUDE_CONFIG_DIR" {
			value = strings.TrimSpace(entryValue)
		}
	}
	if value == "" {
		return "", nil
	}
	resolved, err := resolveSandboxPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve Claude configuration directory: %w", err)
	}
	return resolved, nil
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
