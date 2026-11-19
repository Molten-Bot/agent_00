package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/githubassets"
)

type assetTestTransport func(*http.Request) (*http.Response, error)

func (f assetTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type assetInspectRunner struct {
	*fakeRunner
	inspect func(execx.Command)
}

func (r *assetInspectRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	if (cmd.Name == "codex" || cmd.Name == "claude") && len(cmd.Args) > 1 {
		r.inspect(cmd)
		return execx.Result{}, nil
	}
	return r.fakeRunner.Run(ctx, cmd)
}

func TestRunDeliversTaskAssetsToAgents(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"codex", "claude"} {
		for _, inaccessible := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inaccessible=%t", harness, inaccessible), func(t *testing.T) {
				t.Parallel()
				cfg := sampleConfig()
				cfg.AgentHarness = harness
				cfg.Assets = []githubassets.Asset{
					{URL: "https://github.com/user-attachments/assets/shot", Name: "shot", Source: "Issue description"},
					{URL: "https://github.com/user-attachments/files/42/spec.pdf", Name: "spec.pdf", Source: "Comment 1"},
				}
				guid := fmt.Sprintf("assets-%s-%t", harness, inaccessible)
				runDir := testRunDir(guid)
				repoDir := filepath.Join(runDir, "repo")
				targetDir := filepath.Join(repoDir, cfg.TargetSubdir)
				branch := "agent00-build-api"
				fake := &fakeRunner{t: t, exps: []expectedRun{
					{cmd: execx.Command{Name: "git", Args: []string{"--version"}}},
					{cmd: execx.Command{Name: "gh", Args: []string{"--version"}}},
					{cmd: execx.Command{Name: harness, Args: []string{"--help"}}},
					{cmd: execx.Command{Name: "gh", Args: []string{"auth", "status"}}},
					{cmd: cloneCommand(cfg, repoDir)},
					{cmd: branchCommand(repoDir, branch)},
					{cmd: pushDryRunCommand(repoDir, branch)},
					{cmd: execx.Command{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com"}}, res: execx.Result{Stdout: "test-credential"}},
				}}
				if !inaccessible {
					fake.exps = append(fake.exps, expectedRun{cmd: statusCommand(repoDir)}, expectedRun{cmd: remoteBranchExistsOnOriginCommand(repoDir, branch)}, expectedRun{cmd: prLookupAnyByHeadCommand(repoDir, branch)})
				}
				called := false
				runner := &assetInspectRunner{fakeRunner: fake, inspect: func(cmd execx.Command) {
					called = true
					if inaccessible {
						t.Fatal("agent started without required attachment")
					}
					for _, name := range []string{"01-shot.png", "02-spec.pdf"} {
						path := filepath.Join(runDir, "task-assets", name)
						if !strings.Contains(cmd.Stdin, path) {
							t.Errorf("agent prompt missing %s", name)
						}
						if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
							t.Errorf("asset missing: %v", err)
						}
					}
					if !slices.Contains(cmd.Args, "--add-dir") || !slices.Contains(cmd.Args, runDir) {
						t.Errorf("agent missing access to assets: %v", cmd.Args)
					}
					if harness == "codex" && (!slices.Contains(cmd.Args, "--image") || !slices.Contains(cmd.Args, filepath.Join(runDir, "task-assets", "01-shot.png"))) {
						t.Errorf("Codex missing native image input: %v", cmd.Args)
					}
					if strings.Contains(cmd.Stdin, "test-credential") {
						t.Fatal("credential included in prompt")
					}
				}}
				h := New(runner)
				h.Workspace = testWorkspaceManager(guid)
				h.TargetDirOK = func(path string) bool { return path == targetDir }
				var logs []string
				h.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
				h.AssetTransport = assetTestTransport(func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Authorization") != "Bearer test-credential" {
						t.Error("missing authentication")
					}
					body := "\x89PNG\r\n\x1a\nimage"
					if strings.HasSuffix(req.URL.Path, ".pdf") {
						body = "%PDF-1.4\nexample"
					}
					status := 200
					if inaccessible {
						status = 404
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}, nil
				})
				res := h.Run(context.Background(), cfg)
				if inaccessible {
					if res.Err == nil || !strings.Contains(res.Err.Error(), "HTTP 404") || called {
						t.Fatalf("missing download failure: %+v", res)
					}
				} else if res.Err != nil || !called {
					t.Fatalf("run err=%v agent called=%t", res.Err, called)
				}
				if len(fake.exps) != 0 {
					t.Fatalf("unconsumed commands: %d", len(fake.exps))
				}
				if strings.Contains(strings.Join(logs, "\n"), "test-credential") {
					t.Fatal("credential logged")
				}
				if _, err := os.Stat(filepath.Join(repoDir, "task-assets")); !os.IsNotExist(err) {
					t.Fatalf("assets entered checkout: %v", err)
				}
			})
		}
	}
}
