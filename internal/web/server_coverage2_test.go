package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// srvcovErrReader always fails to read, simulating a client that disconnects
// mid-upload so handlers exercise their io.ReadAll error branches.
type srvcovErrReader struct{}

func (srvcovErrReader) Read([]byte) (int, error) {
	return 0, errors.New("srvcov: simulated body read failure")
}

// srvcovFailingFlushWriter is an http.ResponseWriter + http.Flusher whose
// Write always fails, used to exercise handleStream's write-error paths
// without depending on real network or wall-clock timing.
type srvcovFailingFlushWriter struct {
	header http.Header
}

func (w *srvcovFailingFlushWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *srvcovFailingFlushWriter) WriteHeader(int) {}

func (w *srvcovFailingFlushWriter) Write([]byte) (int, error) {
	return 0, errors.New("srvcov: simulated write failure")
}

func (w *srvcovFailingFlushWriter) Flush() {}

func TestSrvcovNotifyServerReady(t *testing.T) {
	t.Parallel()

	// Nil channel: must return without blocking or panicking.
	notifyServerReady(nil, nil)
	notifyServerReady(nil, errors.New("boom"))

	// Buffered channel with room: takes the send case.
	ready := make(chan error, 1)
	notifyServerReady(ready, nil)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("notifyServerReady(ready, nil) sent %v, want nil", err)
		}
	default:
		t.Fatal("notifyServerReady(ready, nil) did not send on buffered channel")
	}

	// Full channel: must take the default (non-blocking) branch.
	full := make(chan error, 1)
	full <- errors.New("already queued")
	notifyServerReady(full, errors.New("dropped"))
	if len(full) != 1 {
		t.Fatalf("len(full) = %d, want 1 (send must be dropped via default case)", len(full))
	}
}

func TestSrvcovRunDefaultsLogfWhenNilThenShutsDown(t *testing.T) {
	t.Parallel()

	ready := make(chan error, 1)
	// Bypass NewServer so Logf is left nil, exercising Run's defaulting branch.
	srv := Server{Addr: "127.0.0.1:0", Broker: NewBroker(), Ready: ready}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("Run() ready error = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not report readiness in time")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run(cancel) error = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run(cancel) did not stop in time")
	}
}

func TestSrvcovRunReportsListenError(t *testing.T) {
	t.Parallel()

	ready := make(chan error, 1)
	srv := Server{Addr: "this is not a valid address", Broker: NewBroker(), Ready: ready}

	err := srv.Run(context.Background())
	if err == nil {
		t.Fatal("Run(invalid address) error = nil, want listen error")
	}

	select {
	case readyErr := <-ready:
		if readyErr == nil {
			t.Fatal("Ready channel reported nil error, want listen error")
		}
	default:
		t.Fatal("Ready channel was not notified of the listen error")
	}
}

func TestSrvcovLogfNoopWhenLogfNil(t *testing.T) {
	t.Parallel()

	srv := Server{}
	srv.logf("this must not panic: %d", 1)
}

func TestSrvcovWithCacheControlAndWithGzipNilHandler(t *testing.T) {
	t.Parallel()

	h := withCacheControl(nil, "public, max-age=60")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("withCacheControl(nil) status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("withCacheControl(nil) Cache-Control = %q, want empty (handler never writes headers)", got)
	}

	gzh := withGzip(nil)
	rec = httptest.NewRecorder()
	gzh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("withGzip(nil) status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSrvcovGzipResponseWriterWriteHeaderIsIdempotent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	gzw := &gzipResponseWriter{ResponseWriter: rec}
	gzw.WriteHeader(http.StatusCreated)
	gzw.WriteHeader(http.StatusNotFound)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rec.Code = %d, want %d (second WriteHeader must be ignored)", rec.Code, http.StatusCreated)
	}
}

func TestSrvcovGzipResponseWriterFlushesUnderlyingGzipWriter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	rec := httptest.NewRecorder()
	gzw := &gzipResponseWriter{ResponseWriter: rec, writer: gz}

	if _, err := gzw.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	gzw.Flush()

	if buf.Len() == 0 {
		t.Fatal("Flush() did not flush the underlying gzip.Writer")
	}
	if !rec.Flushed {
		t.Fatal("Flush() did not flush the underlying ResponseWriter")
	}
}

func TestSrvcovGitHubReposRefreshRequestedNilRequest(t *testing.T) {
	t.Parallel()

	if gitHubReposRefreshRequested(nil) {
		t.Fatal("gitHubReposRefreshRequested(nil) = true, want false")
	}
}

func TestSrvcovCachedGitHubReposWithNilCache(t *testing.T) {
	t.Parallel()

	srv := Server{}
	repos, ready := srv.cachedGitHubRepos()
	if ready || repos != nil {
		t.Fatalf("cachedGitHubRepos(nil cache) = (%#v, %v), want (nil, false)", repos, ready)
	}
}

func TestSrvcovResolveAndRefreshGitHubReposFallBackWithoutCache(t *testing.T) {
	t.Parallel()

	var resolveCalls int
	srv := Server{
		ResolveGitHubRepos: func(context.Context) ([]GitHubRepo, error) {
			resolveCalls++
			return []GitHubRepo{{Name: "repo", FullName: "acme/repo"}}, nil
		},
	}

	repos, err := srv.resolveGitHubRepos(context.Background())
	if err != nil {
		t.Fatalf("resolveGitHubRepos(no cache) error = %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/repo" {
		t.Fatalf("resolveGitHubRepos(no cache) = %#v", repos)
	}

	repos, err = srv.refreshGitHubRepos(context.Background())
	if err != nil {
		t.Fatalf("refreshGitHubRepos(no cache) error = %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/repo" {
		t.Fatalf("refreshGitHubRepos(no cache) = %#v", repos)
	}
	if resolveCalls != 2 {
		t.Fatalf("resolve calls = %d, want 2", resolveCalls)
	}
}

func TestSrvcovGitHubRepoCacheStoreIfCurrentSkipsStaleGeneration(t *testing.T) {
	t.Parallel()

	cache := &gitHubRepoCache{}
	generation := cache.beginRefresh()
	cache.beginRefresh() // advances the generation again, making `generation` stale.

	cache.storeIfCurrent(generation, []GitHubRepo{{Name: "stale"}})

	if repos, ready := cache.snapshot(); ready || repos != nil {
		t.Fatalf("snapshot() = (%#v, %v), want (nil, false) after stale storeIfCurrent", repos, ready)
	}
}

func TestSrvcovPreloadGitHubReposLogsResolveFailure(t *testing.T) {
	t.Parallel()

	logged := make(chan string, 1)
	srv := Server{
		gitHubRepos: &gitHubRepoCache{},
		ResolveGitHubRepos: func(context.Context) ([]GitHubRepo, error) {
			return nil, errors.New("github preload boom")
		},
		Logf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if strings.Contains(msg, "preload_github_repos") {
				select {
				case logged <- msg:
				default:
				}
			}
		},
	}

	srv.preloadGitHubRepos()

	select {
	case msg := <-logged:
		if !strings.Contains(msg, "github preload boom") {
			t.Fatalf("logged message = %q, want it to contain the resolve error", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for preloadGitHubRepos to log the resolve failure")
	}
}

func TestSrvcovGithubRepoActivityTimeFallsBackToZeroForInvalidTimestamps(t *testing.T) {
	t.Parallel()

	if got := githubRepoActivityTime(GitHubRepo{}); !got.IsZero() {
		t.Fatalf("githubRepoActivityTime(empty) = %v, want zero time", got)
	}
	if got := githubRepoActivityTime(GitHubRepo{PushedAt: "not-a-time", UpdatedAt: "also-not-a-time"}); !got.IsZero() {
		t.Fatalf("githubRepoActivityTime(invalid) = %v, want zero time", got)
	}
}

func TestSrvcovSortGitHubReposByActivityBreaksTiesByName(t *testing.T) {
	t.Parallel()

	repos := []GitHubRepo{
		{FullName: "acme/zebra"},
		{FullName: "acme/apple"},
	}
	sortGitHubReposByActivity(repos)
	if repos[0].FullName != "acme/apple" || repos[1].FullName != "acme/zebra" {
		t.Fatalf("sortGitHubReposByActivity(equal activity) = %#v, want alphabetical tie-break", repos)
	}
}

func TestSrvcovNewGitHubAPIRequestRejectsInvalidMethod(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	if _, err := newGitHubAPIRequest(context.Background(), "BAD METHOD", "https://api.github.com"); err == nil {
		t.Fatal("newGitHubAPIRequest(invalid method) error = nil, want error")
	}
}

func TestSrvcovResolveAuthenticatedGitHubProfileURLEmptyMessageFallsBackToStatus(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return githubProfileResponse(http.StatusForbidden, `{}`), nil
		}),
	}
	_, err := resolveAuthenticatedGitHubProfileURL(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "github api status=403") {
		t.Fatalf("resolveAuthenticatedGitHubProfileURL(empty message) error = %v, want status fallback message", err)
	}
}

func TestSrvcovResolveAuthenticatedGitHubReposEmptyMessageFallsBackToStatus(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return githubResponse(http.StatusForbidden, `{}`, nil), nil
		}),
	}
	_, err := resolveAuthenticatedGitHubRepos(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "github api status=403") {
		t.Fatalf("resolveAuthenticatedGitHubRepos(empty message) error = %v, want status fallback message", err)
	}
}

type srvcovErrReadCloser struct{}

func (srvcovErrReadCloser) Read([]byte) (int, error) { return 0, errors.New("srvcov: read body failed") }
func (srvcovErrReadCloser) Close() error              { return nil }

func TestSrvcovResolveAuthenticatedGitHubReposReadBodyError(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       srvcovErrReadCloser{},
			}, nil
		}),
	}
	_, err := resolveAuthenticatedGitHubRepos(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "read github repositories response") {
		t.Fatalf("resolveAuthenticatedGitHubRepos(body read error) error = %v, want read error", err)
	}
}

func TestSrvcovResolveAuthenticatedGitHubReposDecodeError(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("GITHUB_TOKEN", "")

	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return githubResponse(http.StatusOK, `not json at all`, nil), nil
		}),
	}
	_, err := resolveAuthenticatedGitHubRepos(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "decode github repositories") {
		t.Fatalf("resolveAuthenticatedGitHubRepos(decode error) error = %v, want decode error", err)
	}
}

func TestSrvcovHandlePromptSubmitBodyReadError(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		t.Fatal("submit must not be called when the body fails to read")
		return "", nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/local-prompt", nil)
	req.Body = &srvcovNopCloserReader{srvcovErrReader{}}
	rec := httptest.NewRecorder()
	srv.handleLocalPrompt(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleLocalPrompt(body read error) status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read request body") {
		t.Fatalf("handleLocalPrompt(body read error) body = %s, want read request body error", rec.Body.String())
	}
}

func TestSrvcovHandleReviewSettingsPostBodyReadError(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.ConfigureReviewSettings = func(context.Context, ReviewSettingsRequest) (ReviewSettingsState, error) {
		t.Fatal("configure must not be called when the body fails to read")
		return ReviewSettingsState{}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/review-settings", nil)
	req.Body = &srvcovNopCloserReader{srvcovErrReader{}}
	rec := httptest.NewRecorder()
	srv.handleReviewSettings(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleReviewSettings(body read error) status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestSrvcovHandleAgentAuthConfigureMethodNotAllowedAndBodyReadError(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.ConfigureAgentAuth = func(context.Context, string) (AgentAuthState, error) {
		t.Fatal("configure must not be reached via GET or a failing body read")
		return AgentAuthState{}, nil
	}

	rec := httptest.NewRecorder()
	srv.handleAgentAuthConfigure(rec, httptest.NewRequest(http.MethodGet, "/api/agent-auth/configure", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("handleAgentAuthConfigure(GET) status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/agent-auth/configure", nil)
	req.Body = &srvcovNopCloserReader{srvcovErrReader{}}
	rec = httptest.NewRecorder()
	srv.handleAgentAuthConfigure(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleAgentAuthConfigure(body read error) status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// srvcovNopCloserReader adapts an io.Reader without a Close method into an
// io.ReadCloser so it can be assigned directly to http.Request.Body.
type srvcovNopCloserReader struct {
	srvcovErrReader
}

func (srvcovNopCloserReader) Close() error { return nil }

func TestSrvcovHandleSpeechStatusMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	rec := httptest.NewRecorder()
	srv.handleSpeechStatus(rec, httptest.NewRequest(http.MethodPost, "/api/speech/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("handleSpeechStatus(POST) status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestSrvcovHandleTaskActionPathEdgeCases(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())

	cases := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "no api/tasks prefix", path: "/other", wantStatus: http.StatusNotFound},
		{name: "empty request id and action", path: "/api/tasks/", wantStatus: http.StatusNotFound},
		{name: "unmatched action suffix", path: "/api/tasks/req-1/unknown-action", wantStatus: http.StatusNotFound},
		{name: "invalid percent-encoding", path: "/api/tasks/%zz/close", wantStatus: http.StatusBadRequest, wantBody: "invalid request id"},
		{name: "decoded request id is blank", path: "/api/tasks//close", wantStatus: http.StatusBadRequest, wantBody: "request id is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/tasks/placeholder", nil)
			req.URL.Path = tc.path
			rec := httptest.NewRecorder()
			srv.handleTaskAction(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("handleTaskAction(%q) status = %d, want %d, body=%s", tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("handleTaskAction(%q) body = %s, want contains %q", tc.path, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestSrvcovHandleTaskControlWithMetaHandlerNilAndGenericError(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())

	rec := httptest.NewRecorder()
	srv.handleTaskControlWithMeta(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/req-1/pause", nil), "req-1", "pause", "paused", nil, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("handleTaskControlWithMeta(nil handler) status = %d, want %d", rec.Code, http.StatusNotImplemented)
	}
	if !strings.Contains(rec.Body.String(), "task pause is unavailable") {
		t.Fatalf("handleTaskControlWithMeta(nil handler) body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler := func(context.Context, string) error { return errors.New("underlying failure") }
	srv.handleTaskControlWithMeta(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/req-1/pause", nil), "req-1", "pause", "paused", handler, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("handleTaskControlWithMeta(generic error) status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if !strings.Contains(rec.Body.String(), "underlying failure") {
		t.Fatalf("handleTaskControlWithMeta(generic error) body = %s, want underlying error message", rec.Body.String())
	}
}

func TestSrvcovTaskControlUnavailableResponse(t *testing.T) {
	t.Parallel()

	// No broker configured: caller must fall back to a generic 404.
	noBroker := Server{}
	if status, msg := noBroker.taskControlUnavailableResponse("req-x", "run"); status != 0 || msg != "" {
		t.Fatalf("taskControlUnavailableResponse(no broker) = (%d, %q), want (0, \"\")", status, msg)
	}

	b := NewBroker()
	srv := NewServer("", b)

	// Broker present but the task is unknown.
	if status, msg := srv.taskControlUnavailableResponse("req-missing", "run"); status != 0 || msg != "" {
		t.Fatalf("taskControlUnavailableResponse(missing task) = (%d, %q), want (0, \"\")", status, msg)
	}

	statusCases := []struct {
		logLine     string
		requestID   string
		action      string
		wantStatus  int
		wantMessage string
	}{
		// Note: a "duplicate" status case is intentionally not exercised here.
		// Broker.IngestLog drops "status=duplicate" lines before they ever
		// reach task-state updates (see isDuplicateTaskLifecycleLine in
		// broker.go), so that terminal status is not reachable through the
		// public Broker API today; covering it would require reaching into
		// Broker's unexported internals, which we avoid per task scope.
		{logLine: "dispatch status=completed request_id=req-completed", requestID: "req-completed", action: "run", wantStatus: http.StatusConflict, wantMessage: "task is already completed"},
		{logLine: "dispatch status=no_changes request_id=req-no-changes", requestID: "req-no-changes", action: "run", wantStatus: http.StatusConflict, wantMessage: "task is already completed"},
		{logLine: "dispatch status=stopped request_id=req-stopped", requestID: "req-stopped", action: "run", wantStatus: http.StatusConflict, wantMessage: "task is already stopped"},
		{logLine: `dispatch status=error request_id=req-errored err="boom"`, requestID: "req-errored", action: "run", wantStatus: http.StatusConflict, wantMessage: "task is already finished"},
		{logLine: "dispatch status=invalid request_id=req-invalid", requestID: "req-invalid", action: "run", wantStatus: http.StatusConflict, wantMessage: "task is already finished"},
		{logLine: "dispatch status=start request_id=req-running", requestID: "req-running", action: "", wantStatus: http.StatusConflict, wantMessage: "task control is unavailable for this task"},
	}
	for _, tc := range statusCases {
		b.IngestLog(tc.logLine)
		status, msg := srv.taskControlUnavailableResponse(tc.requestID, tc.action)
		if status != tc.wantStatus || msg != tc.wantMessage {
			t.Fatalf("taskControlUnavailableResponse(%q, %q) = (%d, %q), want (%d, %q)", tc.requestID, tc.action, status, msg, tc.wantStatus, tc.wantMessage)
		}
	}
}

func TestSrvcovHandleTaskRerunBrokerNil(t *testing.T) {
	t.Parallel()

	srv := Server{}
	rec := httptest.NewRecorder()
	srv.handleTaskRerun(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/req-1/rerun", nil), "req-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleTaskRerun(nil broker) status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSrvcovHandleTaskRerunGenericSubmitError(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-rerun-generic-error"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"x","prompt":"x"}`))

	srv := NewServer("", b)
	srv.SubmitLocalPrompt = func(context.Context, []byte) (string, error) {
		return "", errors.New("submit exploded")
	}

	rec := httptest.NewRecorder()
	srv.handleTaskRerun(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+requestID+"/rerun", nil), requestID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleTaskRerun(generic submit error) status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "submit exploded") {
		t.Fatalf("handleTaskRerun(generic submit error) body = %s", rec.Body.String())
	}
}

func TestSrvcovHandleTaskRerunLogsCloseSourceFailure(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	requestID := "req-rerun-close-fails"
	b.RecordTaskRunConfig(requestID, []byte(`{"repo":"x","prompt":"x"}`))
	b.IngestLog("dispatch status=start request_id=" + requestID)
	b.IngestLog("dispatch status=completed request_id=" + requestID)

	srv := NewServer("", b)
	srv.SubmitTaskRerun = func(_ context.Context, rerunOf string, _ []byte, _ bool) (string, error) {
		// Close the source task as a side effect of submission so the
		// handler's own attempt to close it afterwards observes
		// ErrTaskNotFound and must log (rather than fail) the cleanup error.
		if err := b.CloseTask(rerunOf); err != nil {
			t.Fatalf("pre-closing source task: %v", err)
		}
		return "new-request-id", nil
	}

	var logged []string
	srv.Logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	rec := httptest.NewRecorder()
	srv.handleTaskRerun(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+requestID+"/rerun", nil), requestID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("handleTaskRerun(close source fails) status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var sawCloseWarning bool
	for _, line := range logged {
		if strings.Contains(line, "task_rerun_close_source") {
			sawCloseWarning = true
		}
	}
	if !sawCloseWarning {
		t.Fatalf("logged lines = %#v, want a task_rerun_close_source warning", logged)
	}
}

func TestSrvcovHandleTaskRemovalBrokerNilAndCloseCallbackErrorIsLogged(t *testing.T) {
	t.Parallel()

	nilBroker := Server{}
	rec := httptest.NewRecorder()
	nilBroker.handleTaskRemoval(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/req-1/close", nil), "req-1", false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleTaskRemoval(nil broker) status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	b := NewBroker()
	requestID := "req-close-callback-fails"
	b.IngestLog("dispatch status=start request_id=" + requestID)
	b.IngestLog("dispatch status=completed request_id=" + requestID)

	srv := NewServer("", b)
	var logged []string
	srv.Logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	srv.CloseTask = func(context.Context, string) error {
		return errors.New("cleanup callback failed")
	}

	rec = httptest.NewRecorder()
	srv.handleTaskRemoval(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+requestID+"/close", nil), requestID, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleTaskRemoval(close callback error) status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var sawCleanupWarning bool
	for _, line := range logged {
		if strings.Contains(line, "task_close_cleanup") {
			sawCleanupWarning = true
		}
	}
	if !sawCleanupWarning {
		t.Fatalf("logged lines = %#v, want a task_close_cleanup warning", logged)
	}
}

func TestSrvcovCompactStreamTaskLogsShortSlicePassesThrough(t *testing.T) {
	t.Parallel()

	logs := []TaskLog{{Text: "a"}, {Text: "b"}}
	got := compactStreamTaskLogs(logs)
	if len(got) != len(logs) {
		t.Fatalf("compactStreamTaskLogs(short slice) len = %d, want %d", len(got), len(logs))
	}
}

func TestSrvcovCompactStreamTaskLogsDedupesRepeatedProgressSignals(t *testing.T) {
	t.Parallel()

	head := []TaskLog{
		{Text: "stage=clone action=start"},
		{Text: "stage=clone action=start"}, // duplicate signal key: must be skipped
		{Text: "plain log line without a stage marker"},
		{Text: "stage=build action=compile"},
	}
	tail := make([]TaskLog, maxStreamTaskLogs)
	for i := range tail {
		tail[i] = TaskLog{Text: fmt.Sprintf("tail-%d", i)}
	}
	logs := append(append([]TaskLog(nil), head...), tail...)

	got := compactStreamTaskLogs(logs)
	if len(got) != 2+len(tail) {
		t.Fatalf("compactStreamTaskLogs() len = %d, want %d", len(got), 2+len(tail))
	}
	if got[0].Text != "stage=clone action=start" || got[1].Text != "stage=build action=compile" {
		t.Fatalf("compactStreamTaskLogs() compacted head = %#v, want deduped unique stage signals", got[:2])
	}
}

func TestSrvcovTaskProgressSignalLogKeyBlankStageIsIgnored(t *testing.T) {
	t.Parallel()

	if _, ok := taskProgressSignalLogKey("stage= action=foo"); ok {
		t.Fatal("taskProgressSignalLogKey(blank stage) ok = true, want false")
	}
	if _, ok := taskProgressSignalLogKey("no stage marker here"); ok {
		t.Fatal("taskProgressSignalLogKey(no stage marker) ok = true, want false")
	}
}

func TestSrvcovHandleStreamReturnsWhenInitialSnapshotWriteFails(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	w := &srvcovFailingFlushWriter{}
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleStream(w, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleStream did not return promptly after the initial snapshot write failed")
	}
}
