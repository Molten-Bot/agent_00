package githubutil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type cliRunner func(context.Context, execx.Command) (execx.Result, error)

func (f cliRunner) Run(ctx context.Context, cmd execx.Command) (execx.Result, error) {
	return f(ctx, cmd)
}

func TestCLIThrottleSharesObservedQuotaAcrossCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reset := time.Now().Add(time.Minute)
		var calls atomic.Int32
		runner := cliRunner(func(_ context.Context, _ execx.Command) (execx.Result, error) {
			remaining := 0
			if calls.Add(1) > 1 {
				remaining = 4999
				reset = time.Now().Add(time.Hour)
			}
			return execx.Result{Stdout: "[]", Stderr: fmt.Sprintf(
				"* Request to https://api.github.com/repos/acme/repo/issues\n< HTTP/2.0 200 OK\n< X-RateLimit-Resource: core\n< X-RateLimit-Limit: 5000\n< X-RateLimit-Remaining: %d\n< X-RateLimit-Reset: %d\n\n[]\n* Request took 1ms\n",
				remaining, reset.Unix())}, nil
		})
		c := &Cooldown{Logf: t.Logf}
		cmd := execx.Command{Name: "gh", Args: []string{"api", "repos/acme/repo/issues"}}
		failures := make(chan error, 3)
		var workers sync.WaitGroup
		for _, caller := range []string{"issue_admission", "workflow_review", "worker_checks"} {
			workers.Go(func() {
				_, err := c.Run(WithCaller(t.Context(), caller), runner, cmd, false)
				failures <- err
			})
		}
		workers.Wait()
		close(failures)
		var limited int
		for err := range failures {
			if err == nil {
				continue
			}
			var rate *RateLimitError
			if !errors.As(err, &rate) || !rate.RetryAt.Equal(reset) {
				t.Fatal(err)
			}
			limited++
		}
		if limited != 2 || calls.Load() != 1 || c.Status().Core.Remaining != 0 {
			t.Fatalf("rate-limited workers=%d HTTP requests=%d quota=%+v", limited, calls.Load(), c.Status().Core)
		}
		time.Sleep(time.Minute)
		if _, err := c.Run(t.Context(), runner, cmd, false); err != nil || calls.Load() != 2 {
			t.Fatalf("request after reset calls=%d err=%v", calls.Load(), err)
		}
	})
}

func TestCLIWorkflowDiscoveryPacesEachRESTPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var times []time.Time
		var logs []string
		c := &Cooldown{Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
		runner := cliRunner(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
			times = append(times, time.Now())
			endpoint := cmd.Args[1]
			link, bucket := "", "core"
			switch endpoint {
			case "repos/acme/repo/pulls?per_page=100":
				link = "< Link: <https://api.github.com/repositories/42/pulls?per_page=100&page=2>; rel=\"next\"\n"
			case "repositories/42/pulls?per_page=100&page=2":
			case "graphql":
				bucket = "graphql"
			default:
				t.Fatalf("unexpected request %v", cmd.Args)
			}
			return execx.Result{Stdout: "[]", Stderr: fmt.Sprintf(
				"* Request to https://api.github.com/%s\n< HTTP/2.0 200 OK\n< X-RateLimit-Resource: %s\n%s\n[]\n* Request took 1ms\n",
				endpoint, bucket, link)}, nil
		})
		ctx := WithCaller(t.Context(), "workflow_discovery")
		result, err := c.Run(ctx, runner, execx.Command{Name: "gh", Args: []string{"api", "--paginate", "repos/acme/repo/pulls?per_page=100"}}, true)
		if err != nil || result.Stdout != "[]\n[]\n" {
			t.Fatalf("pages = %+v, %v", result, err)
		}
		for _, endpoint := range []string{"graphql", "graphql"} {
			if _, err := c.Run(ctx, runner, execx.Command{Name: "gh", Args: []string{"api", endpoint}}, false); err != nil {
				t.Fatal(err)
			}
		}
		want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 5 * time.Second}
		for i, when := range times {
			if got := when.Sub(start); got != want[i] {
				t.Fatalf("request %d started at %s, want %s", i, got, want[i])
			}
		}
		if len(logs) != 4 || !strings.Contains(logs[0], "caller=workflow_discovery bucket=core") ||
			!strings.Contains(logs[2], "caller=workflow_discovery bucket=graphql") {
			t.Fatalf("request counters: %v", logs)
		}
	})
}

func TestCLITraceRedactsSubprocessFailureAndCountsSecondaryLimit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh CLI unavailable")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Unrelated-Header", "fixture-private-header")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("Retry-After", "23")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"secondary rate limit"}`)
	}))
	defer server.Close()
	now := time.Now()
	c := &Cooldown{Logf: t.Logf, Now: func() time.Time { return now }}
	cmd := execx.Command{Name: "gh", Args: []string{"api", server.URL + "/fixture-private-url"},
		Env: []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + t.TempDir(), "GH_TOKEN=local-fixture"}}
	result, err := c.Run(WithCaller(t.Context(), "worker_review"), execx.OSRunner{}, cmd, false)
	var rate *RateLimitError
	if !errors.As(err, &rate) || !rate.Rejected || !rate.RetryAt.Equal(now.Add(23*time.Second)) {
		t.Fatalf("retry = %v, stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
	visible := fmt.Sprint(result.Stderr, rate.Cause)
	if strings.Contains(visible, "fixture-private") || strings.Contains(visible, "local-fixture") {
		t.Fatalf("raw request trace escaped: %s", visible)
	}
	if _, err := c.Run(t.Context(), execx.OSRunner{}, cmd, false); !errors.As(err, &rate) || calls.Load() != 1 {
		t.Fatalf("got %v after %d calls", err, calls.Load())
	}
}
