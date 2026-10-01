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
		// Permission bypass alone does not enforce a filesystem boundary.
		// Sandbox the entire Claude CLI and its child processes.
		args := []string{"sandbox", "-c", `sandbox_mode="workspace-write"`}
		args = append(args, workspaceSandboxArgs(opts)...)
		args = append(args, "-c", "sandbox_workspace_write.network_access=true", "--", cmd.Name)
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
			cmd.Env = append(cmd.Env, "CODEX_HOME="+filepath.Join(opts.WorkspaceDir, ".moltenhub-agent-io", "config", "sandbox"))
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
	args := []string{"exec", "--sandbox", "workspace-write", "-c", `approval_policy="never"`}
	args = append(args, workspaceSandboxArgs(opts)...)
	if opts.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	for _, dir := range opts.WritableDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		args = append(args, "--add-dir", dir)
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
	args = append(args, "--", prompt)
	return execx.Command{Dir: targetDir, Args: args, Env: append([]string(nil), opts.Env...)}, nil
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
