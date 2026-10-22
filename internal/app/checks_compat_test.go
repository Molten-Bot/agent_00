package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestChecksSnapshotOldGitHubCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		output   string
		queryErr error
		passing  bool
		wantErr  bool
	}{
		{name: "passing", output: "build\tpass\t1s\thttps://example.com/check", passing: true},
		{name: "failed", output: "build\tfail\t1s\thttps://example.com/check", queryErr: errors.New("exit status 1")},
		{name: "pending", output: "build\tpending\t0\thttps://example.com/check", queryErr: errors.New("exit status 8")},
		{name: "query failure", queryErr: errors.New("authentication failed"), wantErr: true},
		{name: "partial output with error", output: "build\tpass\t1s\thttps://example.com/check", queryErr: errors.New("network error"), wantErr: true},
		{name: "empty output", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, required := range []bool{true, false} {
				for _, reconcile := range []bool{true, false} {
					repo := repoWorkspace{Dir: "/repo", PRURL: "https://github.com/acme/repo/pull/42"}
					fake := &fakeRunner{t: t, exps: []expectedRun{
						{cmd: prChecksJSONCommand(repo.Dir, repo.PRURL, required), res: execx.Result{Stderr: "unknown flag: --json\nUsage: gh pr checks [flags]"}, err: errors.New("exit status 1")},
						{cmd: prChecksTextCommand(repo.Dir, repo.PRURL, required), res: execx.Result{Stdout: tc.output}, err: tc.queryErr},
					}}
					h := New(fake)
					query := h.latestChecksAreAllPassing
					if reconcile {
						query = h.reconcileChecksAfterFailure
					}
					passing, summary, err := query(context.Background(), repo, required)
					if passing != tc.passing || (err != nil) != tc.wantErr {
						t.Fatalf("required=%v reconcile=%v: got passing=%v err=%v", required, reconcile, passing, err)
					}
					if err == nil && !strings.Contains(summary, "build\t") {
						t.Fatalf("missing check summary: %q", summary)
					}
					if len(fake.exps) != 0 {
						t.Fatal("text snapshot was not queried")
					}
				}
			}
		})
	}
}
