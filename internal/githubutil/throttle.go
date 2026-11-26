package githubutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

type RateLimitError struct {
	Cause   error
	RetryAt time.Time
	// Rejected indicates a locally gated command or a single API request
	// that GitHub explicitly rejected before doing any work.
	Rejected bool
}

func (e *RateLimitError) Error() string {
	return "GitHub API rate limited; retry at " + e.RetryAt.UTC().Format(time.RFC3339)
}
func (e *RateLimitError) Unwrap() error { return e.Cause }

// Cooldown is shared by pollers using the same GitHub credentials. Pollers
// retry on their normal ticks. Never replay a possibly mutating gh command.
type Cooldown struct {
	mu               sync.Mutex
	until            time.Time
	delay            time.Duration
	budgets          map[string]Budget
	primary          map[string]time.Time
	cache            map[string]cachedResponse
	installationOnce sync.Once
	installation     *Cooldown
	serial           chan struct{}
	discoveryNext    time.Time

	// Logf enables buffered GitHub CLI HTTP tracing. Only counters and
	// GitHub rate-limit metadata leave the coordinator.
	Logf func(string, ...any)
	Now  func() time.Time
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
	if until := c.deadlineLocked(""); now.Before(until) {
		status.RetryAt = &until
	}
	if budget := c.budgets["core"]; now.Before(budget.ResetAt) {
		status.Core = &budget
		status.DiscoveryPaused = budget.Remaining <= discoveryReserve(budget.Limit)
	}
	return status
}

func (c *Cooldown) CheckDiscovery() error {
	status := c.Status()
	if status.RetryAt != nil {
		return &RateLimitError{RetryAt: *status.RetryAt}
	}
	if status.DiscoveryPaused {
		return &RateLimitError{RetryAt: status.Core.ResetAt}
	}
	return nil
}

func discoveryReserve(limit int) int { return max(1, min(25, limit/100)) }

func (c *Cooldown) observe(headers http.Header) {
	resource := headers.Get("X-RateLimit-Resource")
	if c == nil || (resource != "core" && resource != "graphql" && resource != "search" && resource != "code_search") {
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
	old := c.budgets[resource]
	if resetAt.Before(old.ResetAt) || (resetAt.Equal(old.ResetAt) && remaining > old.Remaining) {
		return
	}
	if c.budgets == nil {
		c.budgets = make(map[string]Budget)
	}
	c.budgets[resource] = Budget{Remaining: remaining, Limit: limit, ResetAt: resetAt}
	if remaining == 0 {
		c.pausePrimary(resource, resetAt)
	}
}

func (c *Cooldown) RetryAt() time.Time {
	return c.retryAt("")
}

func (c *Cooldown) retryAt(resource string) time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if until := c.deadlineLocked(resource); c.clock().Before(until) {
		return until
	}
	return time.Time{}
}

func (c *Cooldown) deadlineLocked(resource string) time.Time {
	return latestTime(c.until, latestTime(c.primary["core"], c.primary[resource]))
}

func latestTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (c *Cooldown) pausePrimary(resource string, until time.Time) {
	switch resource {
	case "graphql", "search", "code_search":
	default:
		resource = "core"
	}
	if c.primary == nil {
		c.primary = make(map[string]time.Time)
	}
	c.primary[resource] = latestTime(c.primary[resource], until)
}

// ReviewInstallation isolates the quota for the separate review-check GitHub App.
func (c *Cooldown) ReviewInstallation() *Cooldown {
	if !c.TracksRequests() {
		return c
	}
	c.installationOnce.Do(func() {
		c.installation = &Cooldown{Logf: c.Logf, Now: c.Now}
	})
	return c.installation
}

// Wait lets an active worker preserve its current work while waiting out a
// definite rejection. Background pollers return instead and retry on a tick.
func (c *Cooldown) Wait(ctx context.Context, until time.Time) error {
	timer := time.NewTimer(until.Sub(c.clock()))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type requestClassKey struct{}
type serialOwnerKey struct{}
type cliTraceKey struct{}

func HasCLITrace(ctx context.Context) bool {
	return ctx.Value(cliTraceKey{}) == true
}

func (c *Cooldown) TracksRequests() bool {
	return c != nil && c.Logf != nil
}

// WithCaller labels requests without including repository names, URLs or arguments.
func WithCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, requestClassKey{}, caller)
}

func WithDefaultCaller(ctx context.Context, caller string) context.Context {
	if ctx.Value(requestClassKey{}) != nil {
		return ctx
	}
	return WithCaller(ctx, caller)
}

// Background discovery has a sustained budget of 30 REST responses or 60
// GraphQL responses per minute. Charge every response, including each page.
const discoveryRequestInterval = 2 * time.Second

func (c *Cooldown) acquire(ctx context.Context, discovery bool) (func(), error) {
	if ctx.Value(serialOwnerKey{}) == c {
		return func() {}, nil
	}
	c.mu.Lock()
	if c.serial == nil {
		c.serial = make(chan struct{}, 1)
	}
	c.mu.Unlock()
	release := func() { <-c.serial }
	for {
		select {
		case c.serial <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !discovery {
			return release, nil
		}
		if err := c.CheckDiscovery(); err != nil {
			release()
			return nil, err
		}
		c.mu.Lock()
		next := c.discoveryNext
		c.mu.Unlock()
		if !c.clock().Before(next) {
			return release, nil
		}
		// Release the permit while waiting so a low-priority scan cannot
		// hold up review, ownership checks, or an interactive request.
		release()
		if err := c.Wait(ctx, next); err != nil {
			return nil, err
		}
	}
}

func (c *Cooldown) Run(ctx context.Context, runner execx.Runner, cmd execx.Command, include bool) (execx.Result, error) {
	if c.TracksRequests() && cmd.Name == "gh" && len(cmd.Args) == 3 && cmd.Args[0] == "api" {
		if cmd.Args[2] == "--paginate" {
			cmd.Args = []string{"api", "--paginate", cmd.Args[1]}
		}
		if cmd.Args[1] == "--paginate" && cmd.Args[2] != "graphql" {
			return c.runRESTPages(ctx, runner, cmd)
		}
	}
	return c.run(ctx, runner, cmd, include, nil)
}

func (c *Cooldown) runRESTPages(ctx context.Context, runner execx.Runner, cmd execx.Command) (execx.Result, error) {
	var body strings.Builder
	seen := make(map[string]bool)
	for endpoint := cmd.Args[2]; endpoint != ""; {
		if seen[endpoint] {
			return execx.Result{}, fmt.Errorf("GitHub REST pagination repeated a page")
		}
		seen[endpoint] = true
		cmd.Args = []string{"api", endpoint}
		var link string
		result, err := c.run(ctx, runner, cmd, false, &link)
		if err != nil {
			return result, err
		}
		body.WriteString(result.Stdout)
		body.WriteByte('\n')
		endpoint = nextRESTPage(link)
	}
	return execx.Result{Stdout: body.String()}, nil
}

func (c *Cooldown) run(ctx context.Context, runner execx.Runner, cmd execx.Command, include bool, link *string) (execx.Result, error) {
	caller, _ := ctx.Value(requestClassKey{}).(string)
	if caller == "" {
		caller = "github_cli"
	}
	discovery := caller == "workflow_discovery" || caller == "pr_monitor"
	trace := c.TracksRequests() && cmd.Name == "gh"
	if trace {
		release, err := c.acquire(ctx, discovery)
		if err != nil {
			return execx.Result{}, err
		}
		defer release()
		// A worker's publication command may synchronously validate issue
		// ownership using this same coordinator.
		ctx = context.WithValue(ctx, serialOwnerKey{}, c)
	}
	if until := c.retryAt(commandResource(cmd)); !until.IsZero() {
		return execx.Result{}, &RateLimitError{RetryAt: until, Rejected: true}
	}
	if discovery {
		if err := c.CheckDiscovery(); err != nil {
			return execx.Result{}, err
		}
	}

	var cacheKey string
	if trace {
		cacheKey, cmd = stableRead(cmd)
		if saved := c.cache[cacheKey]; c.clock().Before(saved.expires) {
			c.Logf("github event=cache_hit caller=%s", caller)
			return execx.Result{Stdout: saved.body}, nil
		}
	}

	if include && !trace && len(cmd.Args) > 0 && cmd.Args[0] == "api" {
		cmd.Args = append([]string{"api", "--include"}, cmd.Args[1:]...)
	}
	if trace {
		env := cmd.Env
		if len(env) == 0 {
			env = os.Environ()
		}
		cmd.Env = slices.DeleteFunc(slices.Clone(env), func(value string) bool {
			return strings.HasPrefix(value, "GH_DEBUG=") || strings.HasPrefix(value, "GH_FORCE_TTY=")
		})
		cmd.Env = append(cmd.Env, "GH_DEBUG=api", "NO_COLOR=1")
		cmd.OmitFailureDetails = true
		ctx = context.WithValue(ctx, cliTraceKey{}, true)
	}
	result, err := runner.Run(ctx, cmd)
	var headers http.Header
	var status int
	singleRequest := cmd.Name == "gh" && len(cmd.Args) > 0 && cmd.Args[0] == "api" && !slices.Contains(cmd.Args, "--paginate")
	if trace {
		raw := result.Stderr
		responses, diagnostic := cliResponses(raw)
		result.Stderr = diagnostic
		if err != nil && diagnostic != "" {
			err = fmt.Errorf("%w (%s)", err, diagnostic)
		}
		failure := err
		var cost time.Duration
		for index, response := range responses {
			c.observe(response.headers)
			bucket := response.bucket()
			if bucket == "graphql" {
				cost += discoveryRequestInterval / 2
			} else if bucket != "other" {
				cost += discoveryRequestInterval
			}
			c.Logf("github event=http_response caller=%s bucket=%s status=%d", caller, bucket, response.status)
			message := ""
			if index == len(responses)-1 && failure != nil {
				message = strings.ToLower(diagnostic)
			}
			classified := c.ClassifyFailure(response.status, response.headers, message, failure)
			var rate *RateLimitError
			if errors.As(classified, &rate) {
				rate.Rejected = singleRequest && len(responses) == 1 && (response.status == 429 || response.status == 403)
				err = rate
			}
		}
		if discovery && cost > 0 {
			c.mu.Lock()
			c.discoveryNext = c.clock().Add(cost)
			c.mu.Unlock()
		}
		if len(responses) == 0 {
			c.Logf("github event=no_http_response caller=%s", caller)
		} else {
			last := responses[len(responses)-1]
			headers, status = last.headers, last.status
		}
	} else {
		result.Stdout, headers, status = splitResponse(result.Stdout)
		if status == 0 {
			_, headers, status = splitResponse(result.Stderr)
		}
		c.observe(headers)
	}
	if err == nil {
		if link != nil {
			if status == 0 {
				return execx.Result{}, fmt.Errorf("GitHub REST pagination response headers are unavailable")
			}
			*link = headers.Get("Link")
		}
		if cacheKey != "" && status == http.StatusOK {
			if len(c.cache) >= 512 {
				clear(c.cache)
			}
			if c.cache == nil {
				c.cache = make(map[string]cachedResponse)
			}
			c.cache[cacheKey] = cachedResponse{result.Stdout, c.clock().Add(10 * time.Minute)}
		}
		return result, nil
	}
	var rate *RateLimitError
	if errors.As(err, &rate) {
		return result, err
	}
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(err, contextErr) {
		// os/exec can report "signal: killed" when the caller's deadline terminates gh.
		err = fmt.Errorf("%w: %w", contextErr, err)
	}
	message := strings.ToLower(result.Stderr + "\n" + err.Error())
	if trace && status != 0 {
		// The trace's final response and diagnostic were already classified.
		return result, err
	}
	return result, c.ClassifyFailure(status, headers, message, err)
}

// ClassifyFailure records the retry deadline for a throttled response. Only cause
// is retained, so HTTP callers can classify private bodies without exposing them.
func (c *Cooldown) ClassifyFailure(status int, headers http.Header, message string, cause error) error {
	limited := status == 429 || (status == 403 && (headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") == "0")) ||
		strings.Contains(message, "rate limit exceeded") || strings.Contains(message, "rate limit already exceeded") ||
		strings.Contains(message, "secondary rate limit") || strings.Contains(message, "rate limit was exceeded") || strings.Contains(message, "http 429") ||
		strings.Contains(message, "429 too many requests")
	if !limited {
		return cause
	}

	if c == nil {
		c = &Cooldown{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	retry := headers.Get("Retry-After")
	resetSeconds, resetErr := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64)
	reset := time.Unix(resetSeconds, 0)
	primary := resetErr == nil && reset.After(now) && headers.Get("X-RateLimit-Remaining") == "0"
	if primary && retry == "" && !strings.Contains(message, "secondary rate limit") {
		resource := headers.Get("X-RateLimit-Resource")
		c.pausePrimary(resource, reset)
		return &RateLimitError{Cause: cause, RetryAt: latestTime(reset, c.until)}
	}
	if !now.Before(c.until) {
		// A successful account lookup can precede a throttled scan.
		if now.Sub(c.until) > 15*time.Minute {
			c.delay = 0
		}
		c.delay = min(15*time.Minute, max(time.Minute, 2*c.delay))
	}
	if seconds, parseErr := strconv.ParseUint(retry, 10, 32); parseErr == nil {
		c.extend(now.Add(time.Duration(seconds) * time.Second))
	} else if date, parseErr := http.ParseTime(retry); parseErr == nil {
		c.extend(date)
	}
	if primary {
		c.extend(reset)
	}
	if !now.Before(c.until) {
		jitter := time.Duration(0)
		if c.TracksRequests() && c.Now == nil {
			jitter = rand.N(5 * time.Second)
		}
		c.until = now.Add(c.delay + jitter)
	}
	return &RateLimitError{Cause: cause, RetryAt: c.until}
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
