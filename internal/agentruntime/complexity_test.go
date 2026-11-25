package agentruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type catalogRunner struct {
	t           *testing.T
	reject      bool
	unavailable bool
}

func (r catalogRunner) Run(_ context.Context, cmd execx.Command) (execx.Result, error) {
	if !slices.Equal(cmd.Args, []string{"--version"}) {
		r.t.Fatalf("unexpected version probe %v", cmd.Args)
	}
	return execx.Result{Stdout: "cli 1.2.3\n"}, nil
}
func (r catalogRunner) RunStream(_ context.Context, cmd execx.Command, line execx.StreamLineHandler) (execx.Result, error) {
	if cmd.StdinFile == nil || cmd.Args[0] != "app-server" {
		r.t.Fatal("expected app-server session")
	}
	if err := cmd.StdinFile.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		r.t.Fatal(err)
	}
	scanner := bufio.NewScanner(cmd.StdinFile)
	request := func(method string) map[string]any {
		r.t.Helper()
		if !scanner.Scan() {
			r.t.Fatal("missing request", method)
		}
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil || m["method"] != method {
			r.t.Fatal(m, err)
		}
		return m
	}
	request("initialize")
	line("stdout", `{"id":1,"result":{}}`)
	request("initialized")
	request("model/list")
	if r.unavailable {
		line("stdout", `{"id":2,"error":{"code":-32601,"message":"method unavailable"}}`)
		return execx.Result{}, nil
	}
	if r.reject {
		line("stdout", `{"id":2,"error":{"code":-32600,"message":"provider unavailable"}}`)
		return execx.Result{}, nil
	}
	line("stdout", `{"id":2,"result":{"data":[{"model":"text-only","isDefault":true,"inputModalities":["text"]},{"model":"fast","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}],"nextCursor":"page2"}}`)
	m := request("model/list")
	if m["params"].(map[string]any)["cursor"] != "page2" {
		r.t.Fatalf("missing cursor: %v", m)
	}
	line("stdout", `{"id":3,"result":{"data":[{"model":"deep","supportedReasoningEfforts":[{"reasoningEffort":"xhigh"}]}]}}`)
	return execx.Result{}, context.Canceled
}

func TestComplexitySelectionReachesRuntimeCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness, level, want string
		image                bool
	}{
		{"codex", "easy", "fast", false}, {"codex", "standard", "text-only", false}, {"codex", "advanced", "deep", true},
		{"claude", "easy", "haiku", false}, {"claude", "standard", "sonnet", false}, {"claude", "advanced", "provider-model", false},
	} {
		t.Run(tc.harness+"/"+tc.level, func(t *testing.T) {
			rt, _ := Resolve(tc.harness, "test-cli")
			dir := t.TempDir()
			opts := RunOptions{Env: []string{"CODEX_HOME=" + dir, "ANTHROPIC_DEFAULT_OPUS_MODEL=provider-model"}}
			if tc.image {
				opts.ImagePaths = []string{"image.png"}
			}
			sel, err := rt.ResolveModel(t.Context(), catalogRunner{t: t}, dir, opts, tc.level)
			if err != nil || sel.Model != tc.want || sel.Version != "cli 1.2.3" {
				t.Fatalf("selection=%+v err=%v", sel, err)
			}
			opts.Model, opts.ReasoningEffort = sel.Model, sel.Effort
			cmd, err := rt.BuildCommand(dir, "task prompt", opts)
			i := slices.Index(cmd.Args, "--model")
			if err != nil || i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != tc.want || cmd.Stdin != "task prompt" {
				t.Fatal(cmd.Args, err)
			}
		})
	}
}

func TestCatalogRejectionAndCancellation(t *testing.T) {
	t.Parallel()
	rt := Default()
	dir := t.TempDir()
	opts := RunOptions{Env: []string{"CODEX_HOME=" + dir}}
	if _, err := rt.ResolveModel(t.Context(), catalogRunner{t: t, reject: true}, dir, opts, "easy"); err == nil {
		t.Fatal("provider rejection was ignored")
	}
	selection, err := rt.ResolveModel(t.Context(), catalogRunner{t: t, unavailable: true}, dir, opts, "standard")
	if err != nil || selection.Model != "" || selection.Source != "cli-default-fallback" {
		t.Fatal(selection, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rt.ResolveModel(ctx, catalogRunner{t: t}, dir, opts, "easy"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
