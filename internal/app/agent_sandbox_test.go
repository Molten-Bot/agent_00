package app

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

// Existing workflow fixtures use the task layout produced by testWorkspaceManager.
// Specify that task root independently of the command's runtime environment.
func codexCommand(targetDir, prompt string) execx.Command {
	return codexCommandWithOptions(targetDir, prompt, codexRunOptions{})
}

func codexCommandWithOptions(targetDir, prompt string, opts codexRunOptions) execx.Command {
	rel, err := filepath.Rel(testRunDir(""), targetDir)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
		opts.WorkspaceDir = testRunDir(strings.Split(rel, string(filepath.Separator))[0])
	}
	cmd, err := agentCommandWithOptions(agentruntime.Default(), targetDir, prompt, opts)
	if err != nil {
		panic(err)
	}
	return cmd
}

func expectedCodexArgs(roots []string, extra ...string) []string {
	if roots == nil {
		roots = []string{}
	}
	sort.Strings(roots)
	filesystem := []string{`":root"="read"`}
	for _, root := range roots {
		filesystem = append(filesystem, strconv.Quote(root)+`="write"`)
	}
	args := []string{
		"exec", "-c", `default_permissions="agent00_implementation"`,
		"-c", "permissions.agent00_implementation={filesystem={" + strings.Join(filesystem, ",") + "},network={enabled=true}}",
		"-c", `approval_policy="never"`,
	}
	return append(args, extra...)
}
