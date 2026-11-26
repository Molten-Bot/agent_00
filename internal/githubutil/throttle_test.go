package githubutil

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type responseRunner struct {
	raw   string
	err   error
	calls int
}

func (r *responseRunner) Run(context.Context, execx.Command) (execx.Result, error) {
	r.calls++
	return execx.Result{Stdout: r.raw}, r.err
}

func TestCooldownReportsCommandDeadlineAndProcessError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	killed := errors.New("signal: killed")
	c := &Cooldown{}
	_, err := c.Run(ctx, &responseRunner{err: killed}, execx.Command{Name: "gh"}, false)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, killed) || !c.RetryAt().IsZero() {
		t.Fatalf("deadline error = %v, rate limit retry = %v", err, c.RetryAt())
	}
}

func TestCooldownDeadlinesAndRecovery(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name, headers string
		delay         time.Duration
	}{
		{"primary", "403 Forbidden\r\nX-RateLimit-Remaining: 0\r\nX-RateLimit-Reset: " + fmt.Sprint(now.Add(time.Hour).Unix()), time.Hour},
		{"secondary", "429 Too Many Requests\r\nRetry-After: 20\r\nX-RateLimit-Remaining: 100\r\nX-RateLimit-Reset: " + fmt.Sprint(now.Add(time.Hour).Unix()), 20 * time.Second},
		{"missing timing", "429 Too Many Requests", time.Minute},
		{"permission denied", "403 Forbidden\r\nX-RateLimit-Reset: " + fmt.Sprint(now.Add(time.Hour).Unix()), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := now
			c := Cooldown{Now: func() time.Time { return clock }}
			r := &responseRunner{raw: "HTTP/2.0 " + tc.headers + "\r\n\r\n{}", err: errors.New("exit 1")}
			run := func() error { _, err := c.Run(t.Context(), r, execx.Command{}, true); return err }
			var rate *RateLimitError
			if err := run(); tc.delay == 0 {
				if !errors.Is(err, r.err) || errors.As(err, &rate) {
					t.Fatal(err)
				}
				return
			} else if !errors.As(err, &rate) || !rate.RetryAt.Equal(now.Add(tc.delay)) {
				t.Fatal(err)
			}
			if !errors.As(run(), &rate) || r.calls != 1 {
				t.Fatal("request during cooldown")
			}
			clock = rate.RetryAt
			if tc.name == "missing timing" {
				for _, want := range []time.Duration{2, 4, 8, 15, 15} {
					if !errors.As(run(), &rate) || rate.RetryAt.Sub(clock) != want*time.Minute {
						t.Fatal(rate)
					}
					clock = rate.RetryAt
				}
			}
			r.err = nil
			if err := run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscoveryReservesBudgetForExistingWork(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := Cooldown{Now: func() time.Time { return now }}
	r := &responseRunner{raw: fmt.Sprintf("HTTP/2.0 200 OK\r\n\r\n[1]\nHTTP/2.0 200 OK\r\nX-RateLimit-Resource: core\r\nX-RateLimit-Limit: 5000\r\nX-RateLimit-Remaining: 24\r\nX-RateLimit-Reset: %d\r\n\r\n[2]", now.Add(time.Hour).Unix())}
	result, err := c.Run(t.Context(), r, execx.Command{}, true)
	if err != nil || result.Stdout != "[1]\n[2]" || c.CheckDiscovery() == nil || c.Status().Core.Remaining != 24 {
		t.Fatal(result, err, c.Status())
	}
	if _, err = c.Run(t.Context(), r, execx.Command{}, true); err != nil {
		t.Fatal("reserved requests unavailable", err)
	}
	now = now.Add(time.Hour)
	if err = c.CheckDiscovery(); err != nil {
		t.Fatal(err)
	}
}
