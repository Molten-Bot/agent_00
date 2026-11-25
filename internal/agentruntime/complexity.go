package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func ValidComplexity(level string) bool {
	return level == "easy" || level == "standard" || level == "advanced"
}

type ModelSelection struct {
	Model   string
	Effort  string
	Version string
	Source  string
}

// ResolveModel probes the same executable and environment used for the task.
// CLI aliases remain provider-specific and can be overridden by the account.
func (r Runtime) ResolveModel(ctx context.Context, runner execx.Runner, dir string, opts RunOptions, level string) (ModelSelection, error) {
	var selection ModelSelection
	if !ValidComplexity(level) {
		return selection, fmt.Errorf("unknown task complexity %q", level)
	}
	cmd := execx.Command{Name: r.Command, Dir: dir, Env: opts.Env, Args: []string{"--version"}}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res, err := runner.Run(versionCtx, cmd)
	cancel()
	if err != nil {
		return selection, fmt.Errorf("read %s CLI version: %w", r.Harness, err)
	}
	selection.Version, _, _ = strings.Cut(strings.TrimSpace(res.Stdout), "\n")
	if selection.Version == "" {
		return selection, fmt.Errorf("%s --version returned no version", r.Command)
	}
	if r.Harness == HarnessClaude {
		alias := map[string]string{"easy": "haiku", "standard": "sonnet", "advanced": "opus"}[level]
		selection.Model = alias
		selection.Source = "cli-alias-fallback"
		key := "ANTHROPIC_DEFAULT_" + strings.ToUpper(alias) + "_MODEL="
		for _, entry := range opts.Env {
			if override, ok := strings.CutPrefix(entry, key); ok && override != "" {
				selection.Model = override
				selection.Source = "provider-configuration"
				break
			}
		}
		return selection, nil
	}
	stream, ok := runner.(execx.StreamRunner)
	selection.Source = "cli-default-fallback"
	if !ok {
		return selection, nil
	}
	home, err := CodexHome(opts.Env)
	if err != nil {
		return selection, err
	}
	lock, err := AcquireCodexSession(ctx, home)
	if err != nil {
		return selection, err
	}
	defer lock.Release()
	if err := CheckCodexAuthentication(home); err != nil {
		return selection, err
	}
	cmd.InheritedFiles = lock.ChildFiles()
	models, available, err := codexModels(ctx, stream, cmd)
	if err != nil || !available {
		return selection, err
	}
	model, effort, err := chooseCodexModel(models, level, len(opts.ImagePaths) > 0)
	if err != nil {
		return selection, err
	}
	selection.Model, selection.Effort, selection.Source = model, effort, "model/list"
	return selection, nil
}

type catalogModel struct {
	Model                     string
	Description               string
	IsDefault                 bool
	Hidden                    bool
	InputModalities           []string
	SupportedReasoningEfforts []struct{ ReasoningEffort string }
}

func chooseCodexModel(models []catalogModel, level string, images bool) (string, string, error) {
	preferences := map[string][]string{"easy": {"minimal", "low", "medium"}, "advanced": {"ultra", "max", "xhigh", "high", "medium"}}[level]
	var hints []string
	switch level {
	case "easy":
		hints = []string{"simpler tasks", "easier tasks", "affordable"}
	case "advanced":
		hints = []string{"most capable", "complex coding", "complex tasks"}
	}

	bestScore := -1
	var model, effort string
	for _, m := range models {
		if m.Model == "" || m.Hidden || (images && m.InputModalities != nil && !slices.Contains(m.InputModalities, "image")) {
			continue
		}
		score := 0
		selected := ""
		for _, e := range m.SupportedReasoningEfforts {
			if i := slices.Index(preferences, e.ReasoningEffort); i >= 0 && (len(preferences)-i)*4 > score {
				score = (len(preferences) - i) * 4
				selected = e.ReasoningEffort
			}
		}
		// The API has no numeric capability rank. Prefer the catalog's own
		// task description among models with the same effort support.
		description := strings.ToLower(m.Description)
		for _, hint := range hints {
			if strings.Contains(description, hint) {
				score += 2
				break
			}
		}
		if m.IsDefault {
			score++
		}
		if score > bestScore {
			model, effort, bestScore = m.Model, selected, score
		}
	}
	if model == "" {
		return "", "", fmt.Errorf("Codex model/list returned no compatible model for %s complexity", level)
	}
	return model, effort, nil
}

// Keep stdin open through initialization and pagination. Closing it before the
// response races app-server's shutdown on EOF.
func codexModels(ctx context.Context, runner execx.StreamRunner, cmd execx.Command) ([]catalogModel, bool, error) {
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	input, writer, err := os.Pipe()
	if err != nil {
		return nil, false, err
	}
	defer input.Close()
	defer writer.Close()
	cmd.StdinFile = input
	cmd.Args = []string{"app-server", "--listen", "stdio://"}
	enc := json.NewEncoder(writer)
	err = enc.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "agent00", "version": "1"}}})
	if err != nil {
		return nil, false, err
	}
	var models []catalogModel
	var protocolErr error
	complete := false
	id := 1
	seen := map[string]bool{}
	_, _ = runner.RunStream(probe, cmd, func(stream, line string) {
		if stream != "stdout" || complete || protocolErr != nil {
			return
		}
		var response struct {
			ID    int
			Error *struct {
				Code    int
				Message string
			}
			Result struct {
				Data       []catalogModel
				NextCursor string
			}
		}
		if json.Unmarshal([]byte(line), &response) != nil || response.ID != id {
			return
		}
		if response.Error != nil {
			if response.Error.Code != -32601 {
				protocolErr = fmt.Errorf("Codex model discovery: %s", response.Error.Message)
			}
			cancel()
			return
		}
		params := map[string]any{"limit": 100}
		if id == 1 {
			protocolErr = enc.Encode(map[string]string{"method": "initialized"})
		} else {
			models = append(models, response.Result.Data...)
			cursor := response.Result.NextCursor
			if cursor == "" {
				complete = true
				_ = writer.Close()
				return
			}
			if seen[cursor] {
				protocolErr = fmt.Errorf("Codex model/list repeated a page cursor")
				cancel()
				return
			}
			seen[cursor] = true
			params["cursor"] = cursor
		}
		id++
		if protocolErr == nil {
			protocolErr = enc.Encode(map[string]any{"method": "model/list", "id": id, "params": params})
		}
		if protocolErr != nil {
			cancel()
		}
	})
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if protocolErr != nil {
		return nil, false, protocolErr
	}
	if complete {
		return models, true, nil
	}
	// A missing app-server/method, old CLI, or unavailable discovery service
	// delegates model selection to the configured Codex CLI.
	return nil, false, nil
}
