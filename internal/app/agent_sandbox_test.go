package app

import (
	"encoding/json"
	"path/filepath"
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
	filesystem := []string{`":root"="read"`}
	for _, root := range roots {
		encoded, _ := json.Marshal(root)
		filesystem = append(filesystem, string(encoded)+`="write"`)
	}
	args := []string{
		"exec", "-c", `permission_profile="agent00_task"`,
		"-c", `permissions.agent00_task={filesystem={` + strings.Join(filesystem, ",") + `},network={enabled=true}}`,
		"-c", `approval_policy="never"`,
	}
	return append(args, extra...)
}
