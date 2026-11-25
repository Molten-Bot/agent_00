package githubutil

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type RateLimitError struct {
	Cause   error
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	return "GitHub API rate limited; retry at " + e.RetryAt.UTC().Format(time.RFC3339)
}
func (e *RateLimitError) Unwrap() error { return e.Cause }

// Cooldown is shared by pollers using the same GitHub credentials. Pollers
// retry on their normal ticks. Never replay a possibly mutating gh command.
type Cooldown struct {
	mu    sync.Mutex
	until time.Time
	delay time.Duration
	core  Budget
	Now   func() time.Time
}

func (c *Cooldown) clock() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Budget is the most recently observed core REST quota for this window.
type Budget struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetAt   time.Time `json:"resetAt"`
}
type RateStatus struct {
	Core            *Budget    `json:"core,omitempty"`
	RetryAt         *time.Time `json:"retryAt,omitempty"`
	DiscoveryPaused bool       `json:"discoveryPaused,omitempty"`
}

func (c *Cooldown) Status() RateStatus {
	var status RateStatus
	if c == nil {
		return status
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if now.Before(c.until) {
		until := c.until
		status.RetryAt = &until
	}
	if now.Before(c.core.ResetAt) {
		budget := c.core
		status.Core = &budget
		status.DiscoveryPaused = budget.Remaining <= max(1, min(25, budget.Limit/100))
	}
	return status
}

func (c *Cooldown) CheckDiscovery() error {
	status := c.Status()
	if status.RetryAt != nil {
		return &RateLimitError{RetryAt: *status.RetryAt}
	}
	if status.DiscoveryPaused {
		return fmt.Errorf("GitHub discovery paused with %d of %d REST requests left; resumes after %s", status.Core.Remaining, status.Core.Limit, status.Core.ResetAt.UTC().Format(time.RFC3339))
	}
	return nil
}

func (c *Cooldown) observe(headers http.Header) {
	if c == nil || headers.Get("X-RateLimit-Resource") != "core" {
		return
	}
	limit, e1 := strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	remaining, e2 := strconv.Atoi(headers.Get("X-RateLimit-Remaining"))
	reset, e3 := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || limit <= 0 || remaining < 0 || remaining > limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	resetAt := time.Unix(reset, 0).UTC()
	if resetAt.Before(c.core.ResetAt) || (resetAt.Equal(c.core.ResetAt) && remaining > c.core.Remaining) {
		return
	}
	c.core = Budget{Remaining: remaining, Limit: limit, ResetAt: resetAt}
}

func (c *Cooldown) RetryAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clock().Before(c.until) {
		return c.until
	}
	return time.Time{}
}

func (c *Cooldown) Run(ctx context.Context, runner execx.Runner, cmd execx.Command, include bool) (execx.Result, error) {
	if until := c.RetryAt(); !until.IsZero() {
		return execx.Result{}, &RateLimitError{RetryAt: until}
	}
	if include && len(cmd.Args) > 0 && cmd.Args[0] == "api" {
		cmd.Args = append([]string{"api", "--include"}, cmd.Args[1:]...)
	}
	result, err := runner.Run(ctx, cmd)
	body, headers, status := splitResponse(result.Stdout)
	result.Stdout = body
	c.observe(headers)
	if err == nil {
		return result, nil
	}
	message := strings.ToLower(result.Stderr)
	if message == "" {
		message = strings.ToLower(err.Error())
	}
	limited := status == 429 || (status == 403 && (headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") == "0")) ||
		strings.Contains(message, "rate limit exceeded") || strings.Contains(message, "rate limit already exceeded") ||
		strings.Contains(message, "secondary rate limit") || strings.Contains(message, "http 429")
	if !limited {
		return result, err
	}

	if c == nil {
		c = &Cooldown{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if !now.Before(c.until) {
		// Decay the backoff after a quiet interval. A successful account
		// lookup can precede a throttled scan, so it mustn't reset the delay.
		if now.Sub(c.until) > 15*time.Minute {
			c.delay = 0
		}
		c.delay = min(15*time.Minute, max(time.Minute, 2*c.delay))
	}
	retry := headers.Get("Retry-After")
	if seconds, parseErr := strconv.ParseUint(retry, 10, 32); parseErr == nil {
		c.extend(now.Add(time.Duration(seconds) * time.Second))
	} else if date, parseErr := http.ParseTime(retry); parseErr == nil {
		c.extend(date)
	}
	if seconds, parseErr := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); parseErr == nil && headers.Get("X-RateLimit-Remaining") == "0" {
		c.extend(time.Unix(seconds, 0))
	}
	if !now.Before(c.until) {
		c.until = now.Add(c.delay)
	}
	return result, &RateLimitError{Cause: err, RetryAt: c.until}
}

func (c *Cooldown) extend(deadline time.Time) {
	if deadline.After(c.until) {
		c.until = deadline
	}
}

// gh api --include --paginate emits a header block before each page.
// Only the last page's headers describe the command's final response.
func splitResponse(raw string) (string, http.Header, int) {
	if !strings.HasPrefix(raw, "HTTP/") {
		return raw, nil, 0
	}
	var body []string
	var headers http.Header
	var status int
	inHeaders := false
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "HTTP/") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				status, _ = strconv.Atoi(fields[1])
				headers = make(http.Header)
				inHeaders = true
				continue
			}
		}
		if inHeaders {
			if line == "" {
				inHeaders = false
			} else if k, v, ok := strings.Cut(line, ":"); ok {
				headers.Set(k, strings.TrimSpace(v))
			}
			continue
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n"), headers, status
}
