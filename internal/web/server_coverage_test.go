package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/stringsx"
)

type nonFlushingResponseWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *nonFlushingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *nonFlushingResponseWriter) Write(payload []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(payload)
}

func (w *nonFlushingResponseWriter) WriteHeader(statusCode int) {
	w.code = statusCode
}

type flushingRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushingRecorder) Flush() {
	w.flushed = true
}

func TestHandleStateAndStreamErrorPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", nil)

	rec := httptest.NewRecorder()
	srv.handleState(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleState(nil broker) status = %d, want 503", rec.Code)
	}

	noFlush := &nonFlushingResponseWriter{}
	srv.Broker = NewBroker()
	srv.handleStream(noFlush, httptest.NewRequest(http.MethodGet, "/api/stream", nil))
	if noFlush.code != http.StatusInternalServerError {
		t.Fatalf("handleStream(non flusher) status = %d, want 500", noFlush.code)
	}
}

func TestGzipResponseWriterFlushAndHeaderHelpers(t *testing.T) {
	t.Parallel()

	rec := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	gzw := &gzipResponseWriter{ResponseWriter: rec, writer: nil}
	gzw.Flush()
	if !rec.flushed {
		t.Fatal("Flush() did not flush underlying ResponseWriter")
	}

	header := make(http.Header)
	addVaryHeader(header, "Accept-Encoding")
	addVaryHeader(header, "accept-encoding")
	if got := header.Values("Vary"); len(got) != 1 {
		t.Fatalf("len(Vary) = %d, want 1 (%v)", len(got), got)
	}
	addVaryHeader(header, " ")
	if got := header.Values("Vary"); len(got) != 1 {
		t.Fatalf("blank vary value mutated header: %v", got)
	}
}

func TestRequestAndPathCompressionHelpersAndFirstNonEmptyString(t *testing.T) {
	t.Parallel()

	if requestWantsGzip(nil) {
		t.Fatal("requestWantsGzip(nil) = true, want false")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	if !requestWantsGzip(req) {
		t.Fatal("requestWantsGzip(gzip) = false, want true")
	}
	if isCompressiblePath(" ") {
		t.Fatal("isCompressiblePath(blank) = true, want false")
	}

	if got := stringsx.FirstNonEmpty("", " ", "\t"); got != "" {
		t.Fatalf("stringsx.FirstNonEmpty(all blank) = %q, want empty", got)
	}
}

func TestAgentAuthStatusAndConfigureErrorPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.AgentAuthStatus = func(context.Context) (AgentAuthState, error) {
		return AgentAuthState{Ready: false}, errors.New("status unavailable")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agent-auth", nil)
	srv.handleAgentAuthStatus(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("handleAgentAuthStatus(error) status = %d, want 500", rec.Code)
	}

	srv.ConfigureAgentAuth = func(context.Context, string) (AgentAuthState, error) {
		return AgentAuthState{}, errors.New("configure failed")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/configure", bytes.NewBufferString(`{"value":"token"}`))
	srv.handleAgentAuthConfigure(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleAgentAuthConfigure(callback error) status = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/agent-auth/configure", bytes.NewBufferString(`{"value":" "}`))
	srv.handleAgentAuthConfigure(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("handleAgentAuthConfigure(empty value) status = %d, want 400", rec.Code)
	}
}

func TestCurrentAgentAuthStateFillsDefaultStateFromReadyFlag(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.AgentAuthStatus = func(context.Context) (AgentAuthState, error) {
		return AgentAuthState{Ready: true, State: ""}, nil
	}
	state, err := srv.currentAgentAuthState(context.Background())
	if err != nil {
		t.Fatalf("currentAgentAuthState() error = %v", err)
	}
	if state.State != "ready" {
		t.Fatalf("state = %+v, want ready state", state)
	}

	srv.AgentAuthStatus = func(context.Context) (AgentAuthState, error) {
		return AgentAuthState{Ready: false, State: ""}, nil
	}
	state, err = srv.currentAgentAuthState(context.Background())
	if err != nil {
		t.Fatalf("currentAgentAuthState() error = %v", err)
	}
	if state.State != "needs_device_auth" {
		t.Fatalf("state = %+v, want needs_device_auth", state)
	}

	srv.AgentAuthStatus = nil
	state, err = srv.currentAgentAuthState(context.Background())
	if err != nil {
		t.Fatalf("currentAgentAuthState(nil status) error = %v", err)
	}
	if !state.Ready || state.State != "ready" {
		t.Fatalf("currentAgentAuthState(nil status) = %+v", state)
	}
}

func TestHandleAgentAuthConfigureDecodeFailure(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	srv.ConfigureAgentAuth = func(context.Context, string) (AgentAuthState, error) {
		return AgentAuthState{Ready: true, State: "ready"}, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agent-auth/configure", bytes.NewBufferString(`{"value"`))
	srv.handleAgentAuthConfigure(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("decode failure status = %d, want 400", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response JSON: %v", err)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("ok = true, want false (%v)", body)
	}
}

func TestHandleChatAndHandleGameReportNotFoundForOtherPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())

	rec := httptest.NewRecorder()
	srv.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat/extra", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("handleChat(other path) status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.handleGame(rec, httptest.NewRequest(http.MethodGet, "/game/extra", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("handleGame(other path) status = %d, want 404", rec.Code)
	}
}

func TestInjectBottomDockComponentSkipsDataWithoutPlaceholder(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	data := []byte("<html><body>no dock placeholder here</body></html>")
	got := srv.injectBottomDockComponent(context.Background(), data)
	if string(got) != string(data) {
		t.Fatalf("injectBottomDockComponent(no placeholder) = %q, want unchanged %q", got, data)
	}
}

func TestHandleGitHubReposMethodAndErrorAndRefreshPaths(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/github/repos", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/github/repos status = %d, want 405", resp.Code)
	}

	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		return nil, errors.New("github unavailable")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/github/repos", nil)
	resp = httptest.NewRecorder()
	srv.handleGitHubRepos(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/github/repos (error) status = %d, want 200", resp.Code)
	}
	body := decodeJSONMapFromCoverageTest(t, resp.Body.Bytes())
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("github repos error response ok = %#v, want false", body["ok"])
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "github unavailable") {
		t.Fatalf("github repos error = %q, want github unavailable", got)
	}

	var calls int
	srv.ResolveGitHubRepos = func(context.Context) ([]GitHubRepo, error) {
		calls++
		return []GitHubRepo{{Name: "repo", FullName: "acme/repo"}}, nil
	}
	req = httptest.NewRequest(http.MethodGet, "/api/github/repos?refresh=1", nil)
	resp = httptest.NewRecorder()
	srv.handleGitHubRepos(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/github/repos?refresh=1 status = %d, want 200", resp.Code)
	}
	if calls != 1 {
		t.Fatalf("ResolveGitHubRepos calls = %d, want 1 (refresh must still resolve)", calls)
	}
	body = decodeJSONMapFromCoverageTest(t, resp.Body.Bytes())
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("github repos refresh response ok = %#v, want true", body["ok"])
	}
}

func decodeJSONMapFromCoverageTest(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode json body error = %v (body=%q)", err, raw)
	}
	return decoded
}

func TestHandleReviewSettingsAdditionalBranches(t *testing.T) {
	t.Parallel()

	srv := NewServer("", NewBroker())
	handler := srv.Handler()

	// Method not allowed.
	req := httptest.NewRequest(http.MethodDelete, "/api/review-settings", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/review-settings status = %d, want 405", resp.Code)
	}

	// GET with no status callback falls back to the default state.
	req = httptest.NewRequest(http.MethodGet, "/api/review-settings", nil)
	resp = httptest.NewRecorder()
	srv.handleReviewSettings(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/review-settings (nil status) status = %d, want 200", resp.Code)
	}
	var getBody struct {
		OK       bool                `json:"ok"`
		Settings ReviewSettingsState `json:"settings"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode default settings: %v", err)
	}
	if !getBody.OK || getBody.Settings.MergeMethod != "squash" || getBody.Settings.ReviewLevel != "off" {
		t.Fatalf("default settings = %#v", getBody)
	}

	// GET with a failing status callback reports an error but still includes settings.
	srv.ReviewSettingsStatus = func(context.Context) (ReviewSettingsState, error) {
		return ReviewSettingsState{}, errors.New("load failed")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/review-settings", nil)
	resp = httptest.NewRecorder()
	srv.handleReviewSettings(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("GET /api/review-settings (status error) status = %d, want 500", resp.Code)
	}

	// POST with no configure callback.
	srv.ReviewSettingsStatus = nil
	req = httptest.NewRequest(http.MethodPost, "/api/review-settings", bytes.NewBufferString(`{}`))
	resp = httptest.NewRecorder()
	srv.handleReviewSettings(resp, req)
	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/review-settings (nil configure) status = %d, want 501", resp.Code)
	}

	// POST with malformed JSON body.
	srv.ConfigureReviewSettings = func(_ context.Context, req ReviewSettingsRequest) (ReviewSettingsState, error) {
		return ReviewSettingsState{AutoMerge: req.AutoMerge, MergeMethod: req.MergeMethod, ReviewLevel: req.ReviewLevel}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/api/review-settings", bytes.NewBufferString(`{"auto_merge":`))
	resp = httptest.NewRecorder()
	srv.handleReviewSettings(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/review-settings (malformed json) status = %d, want 400", resp.Code)
	}

	// POST where the configure callback fails.
	srv.ConfigureReviewSettings = func(context.Context, ReviewSettingsRequest) (ReviewSettingsState, error) {
		return ReviewSettingsState{}, errors.New("configure failed")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/review-settings", bytes.NewBufferString(`{"auto_merge":true}`))
	resp = httptest.NewRecorder()
	srv.handleReviewSettings(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/review-settings (configure error) status = %d, want 400", resp.Code)
	}
	var postBody struct {
		OK       bool                `json:"ok"`
		Error    string              `json:"error"`
		Settings ReviewSettingsState `json:"settings"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &postBody); err != nil {
		t.Fatalf("decode configure error response: %v", err)
	}
	if postBody.OK || postBody.Error != "configure failed" {
		t.Fatalf("configure error response = %#v", postBody)
	}
	if postBody.Settings.MergeMethod != "squash" || postBody.Settings.ReviewLevel != "off" {
		t.Fatalf("configure error response fills in defaults = %#v", postBody.Settings)
	}
}

func TestHandleStreamCoalescesUpdatesAndStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	srv := NewServer("", b)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/stream: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readDataLine := func() string {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read stream line: %v", err)
			}
			if strings.HasPrefix(line, "data: ") {
				return line
			}
		}
	}

	// Initial snapshot.
	_ = readDataLine()

	// Ingest a couple of updates in quick succession; the handler must either
	// coalesce them behind a short timer or flush immediately, exercising the
	// "case <-updates" branch (and, after enough wall time, the scheduled
	// timer-fired branch) without relying on the 15s keepalive ticker.
	b.IngestLog("dispatch status=start request_id=req-stream-a")
	b.IngestLog("dispatch status=start request_id=req-stream-b")
	_ = readDataLine()

	time.Sleep(150 * time.Millisecond)
	b.IngestLog("dispatch status=completed request_id=req-stream-a workspace=/tmp/run branch=agent00-a")
	_ = readDataLine()

	cancel()
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				readErr <- err
				return
			}
		}
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("expected stream read to fail after client context cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream to close after client context cancellation")
	}
}
