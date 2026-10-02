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
	if roots == nil {
		roots = []string{}
	}
	encoded, _ := json.Marshal(roots)
	args := []string{
		"exec", "--sandbox", "workspace-write", "-c", `approval_policy="never"`,
		"-c", "sandbox_workspace_write.writable_roots=" + string(encoded),
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
	}
	return append(args, extra...)
}
