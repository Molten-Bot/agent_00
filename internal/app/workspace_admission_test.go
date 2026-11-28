package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/agentruntime"
	"github.com/Molten-Bot/agent_00/internal/execx"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

type modelDiscoveryRunner struct{ sessionRunnerFunc }

func (r modelDiscoveryRunner) RunStream(ctx context.Context, cmd execx.Command, line execx.StreamLineHandler) (execx.Result, error) {
	res, err := r.Run(ctx, cmd)
	line("stdout", `{"id":1,"result":{}}`)
	line("stdout", `{"id":2,"result":{"data":[{"model":"test-model","isDefault":true}]}}`)
	return res, err
}

func TestCodexWaitYieldsWorkerToPostCheckVerification(t *testing.T) {
	t.Parallel()
	for _, discovery := range []bool{true, false} {
		t.Run(fmt.Sprintf("model_discovery=%t", discovery), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			receive := func(ch <-chan string) string {
				t.Helper()
				select {
				case value := <-ch:
					return value
				case <-ctx.Done():
					t.Fatal("timed out waiting for scheduler integration event")
					return ""
				}
			}

			queued := make(chan string, 10)
			scheduler := localruntime.NewAdaptiveScheduler(localruntime.SchedulerConfig{
				MinParallel: 2, MaxParallel: 2,
			}, func(format string, args ...any) {
				if format == "task status=queued request_id=%s queue_depth=%d running=%d allowed=%d%s" {
					queued <- args[0].(string)
				}
			}, localruntime.WithSchedulerDiskSpace(func(string) (string, uint64, uint64, error) {
				return "disk", 8 * workspace.MinimumFreeBytes, 8 * workspace.MinimumFreeBytes, nil
			}))
			root, home := t.TempDir(), t.TempDir()
			admit := func(ctx context.Context, task, dir string, priority int) (func(), error) {
				_, release, err := scheduler.AcquireWorkspace(ctx, task, localruntime.TaskDiskRequest{
					Workspace: workspace.Manager{Root: root}, ExistingDir: dir, Priority: priority,
				}, false)
				return release, err
			}
			activeSlot, err := scheduler.Acquire(ctx, "implementation")
			if err != nil {
				t.Fatal(err)
			}
			defer activeSlot()
			reviewerDir := t.TempDir()
			reviewSlot, err := admit(ctx, "synthetic-review", reviewerDir, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer reviewSlot()

			runtime := agentruntime.Default()
			opts := codexRunOptions{Env: []string{"CODEX_HOME=" + home}}
			activeStarted, activeDone := make(chan string, 1), make(chan string, 1)
			finishActive := make(chan struct{})
			active := New(sessionRunnerFunc(func(ctx context.Context, _ execx.Command) (execx.Result, error) {
				activeStarted <- "started"
				select {
				case <-finishActive:
					return execx.Result{}, nil
				case <-ctx.Done():
					return execx.Result{}, ctx.Err()
				}
			}))
			go func() {
				_, err := active.runCodexWithHeartbeat(ctx, runtime, root, "implement", opts, agentInvocationLogMetadata{})
				activeDone <- fmt.Sprint(err)
			}()
			receive(activeStarted)

			p1Started := make(chan string, 1)
			finishP1 := make(chan struct{})
			p1Dir := t.TempDir()
			go func() {
				release, err := admit(ctx, "p1", p1Dir, 1)
				p1Started <- fmt.Sprint(err)
				if err == nil {
					defer release()
					select {
					case <-ctx.Done():
					case <-finishP1:
					}
				}
			}()
			if got := receive(queued); got != "p1" {
				t.Fatalf("first wait = %s, want P1", got)
			}

			verified, verifierDone := make(chan string, 1), make(chan string, 1)
			finishVerification := make(chan struct{})
			verificationDir := t.TempDir()
			commits := draftPatchCommits{Base: "base-commit", Head: "reviewed-head"}
			verifier := New(sessionRunnerFunc(func(ctx context.Context, cmd execx.Command) (execx.Result, error) {
				if cmd.Dir != verificationDir {
					return execx.Result{}, fmt.Errorf("verification changed checkout: %s", cmd.Dir)
				}
				if cmd.Name == "gh" {
					return execx.Result{Stdout: `{"base":"base-commit","head":"reviewed-head","baseRef":"main","headRef":"task","state":"open","draft":true}`}, nil
				}
				verified <- "confirmed draft base"
				select {
				case <-finishVerification:
					return execx.Result{Stdout: "base-commit\trefs/heads/main\n"}, nil
				case <-ctx.Done():
					return execx.Result{}, ctx.Err()
				}
			}))
			go func() {
				var release func()
				verifier.AcquireWorkspace = func(ctx context.Context) (err error) {
					release, err = admit(ctx, "p0-verification", verificationDir, 2)
					return err
				}
				err := verifier.confirmDraftPatchCommits(ctx, repoWorkspace{
					Dir: verificationDir, PRURL: "https://github.com/example/repo/pull/42",
					BaseBranch: "main", Branch: "task", ReviewedPatch: &draftReviewPatch{commits: commits},
				})
				if release != nil {
					release()
				}
				verifierDone <- fmt.Sprint(err)
			}()
			if got := receive(queued); got != "p0-verification" {
				t.Fatalf("second wait = %s, want P0 verification", got)
			}

			waiting, reviewerDone := make(chan string, 1), make(chan string, 1)
			run := sessionRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
				if cmd.Dir != reviewerDir {
					return execx.Result{}, fmt.Errorf("reviewer changed checkout: %s", cmd.Dir)
				}
				return execx.Result{Stdout: "test-version"}, nil
			})
			var runner execx.Runner = run
			if discovery {
				runner = modelDiscoveryRunner{run}
			}
			reviewer := New(runner)
			reviewer.OnCodexSessionWait = func() func() {
				reviewSlot()
				reviewSlot = nil
				waiting <- "waiting for Codex session"
				return func() {}
			}
			reviewer.AcquireWorkspace = func(ctx context.Context) (err error) {
				reviewSlot, err = admit(ctx, "synthetic-review", reviewerDir, 0)
				return err
			}
			go func() {
				var err error
				if discovery {
					_, err = reviewer.lazyTaskModel(ctx, runtime, reviewerDir, opts, "easy", "review", "resolve_reviewer_model")()
				} else {
					_, err = reviewer.runCodexWithHeartbeat(ctx, runtime, reviewerDir, "review", opts, agentInvocationLogMetadata{})
				}
				if reviewSlot != nil {
					reviewSlot()
				}
				reviewerDone <- fmt.Sprint(err)
			}()
			receive(waiting)
			receive(verified)
			select {
			case <-activeDone:
				t.Fatal("implementation was interrupted")
			case <-p1Started:
				t.Fatal("older P1 bypassed pending P0 verification")
			case <-reviewerDone:
				t.Fatal("reviewer bypassed held Codex lock")
			default:
			}
			close(finishVerification)
			if got := receive(verifierDone); got != "<nil>" {
				t.Fatal(got)
			}
			if got := receive(p1Started); got != "<nil>" {
				t.Fatal(got)
			}
			close(finishActive)
			if got := receive(activeDone); got != "<nil>" {
				t.Fatal(got)
			}
			if got := receive(queued); got != "synthetic-review" {
				t.Fatalf("readmission wait = %s, want synthetic review", got)
			}
			probe, err := agentruntime.AcquireCodexSession(ctx, home)
			if err != nil {
				t.Fatalf("reviewer kept the Codex lock while queued for workspace: %v", err)
			}
			probe.Release()
			close(finishP1)
			if got := receive(reviewerDone); got != "<nil>" {
				t.Fatal(got)
			}
		})
	}
}
