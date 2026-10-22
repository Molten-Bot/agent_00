package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/gitsigning"
)

func (h Harness) runCommand(ctx context.Context, phase string, cmd execx.Command) (execx.Result, error) {
	if h.Signing == nil || cmd.Name != "git" {
		return h.runCommandUnchecked(ctx, phase, cmd)
	}
	cmd.Env = h.Signing.Environment(cmd.Env)
	if len(cmd.Args) == 0 {
		return h.runCommandUnchecked(ctx, phase, cmd)
	}
	operation := cmd.Args[0]
	if operation == "push" {
		trusted := make([]string, 0, len(h.signingHistory[cmd.Dir]))
		for oid := range h.signingHistory[cmd.Dir] {
			trusted = append(trusted, oid)
		}
		sort.Strings(trusted)
		if err := h.Signing.VerifyNew(ctx, h.Runner, cmd.Dir, trusted); err != nil {
			return execx.Result{}, err
		}
	}
	result, err := h.runCommandUnchecked(ctx, phase, cmd)
	if err != nil {
		detail := strings.ToLower(result.Stderr + " " + result.Stdout)
		if strings.Contains(detail, "failed to sign") || strings.Contains(detail, "signing failed") || strings.Contains(detail, "couldn't load public key") {
			return result, fmt.Errorf("%w: Git could not sign the commit; publication stopped", gitsigning.ErrSetup)
		}
		return result, err
	}
	if operation == "clone" {
		repo := cmd.Args[len(cmd.Args)-1]
		if !filepath.IsAbs(repo) && cmd.Dir != "" {
			repo = filepath.Join(cmd.Dir, repo)
		}
		err = h.recordSigningHistory(ctx, repo, false)
	} else if operation == "fetch" {
		err = h.recordSigningHistory(ctx, cmd.Dir, true)
	}
	return result, err
}

// Only clone and harness-controlled fetches establish trusted history. Local task
// commits must not become exempt merely because the agent updates a branch ref.
func (h Harness) recordSigningHistory(ctx context.Context, repo string, fetched bool) error {
	if h.signingHistory == nil {
		return fmt.Errorf("%w: signing history was not initialized", gitsigning.ErrSetup)
	}
	if h.signingHistory[repo] == nil {
		h.signingHistory[repo] = map[string]bool{}
	}
	if !fetched {
		res, err := h.Runner.Run(ctx, execx.Command{Name: "git", Dir: repo, Args: []string{"for-each-ref", "--format=%(objectname)"}})
		if err != nil {
			return fmt.Errorf("%w: cannot capture existing repository history", gitsigning.ErrSetup)
		}
		for _, oid := range strings.Fields(res.Stdout) {
			h.signingHistory[repo][oid] = true
		}
	} else {
		res, err := h.Runner.Run(ctx, execx.Command{Name: "git", Dir: repo, Args: []string{"rev-parse", "--git-path", "FETCH_HEAD"}})
		if err != nil {
			return fmt.Errorf("%w: cannot locate fetched history", gitsigning.ErrSetup)
		}
		path := strings.TrimSpace(res.Stdout)
		if !filepath.IsAbs(path) {
			path = filepath.Join(repo, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: cannot read fetched history", gitsigning.ErrSetup)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				h.signingHistory[repo][fields[0]] = true
			}
		}
	}
	return nil
}
