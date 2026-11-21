package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

type taskAssetTransportFunc func(*http.Request) (*http.Response, error)

func (f taskAssetTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTaskAssetsRenderInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task asset checks")
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{}, fmt.Errorf("unexpected GitHub CLI command")
	}), true)
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "github.com" || req.Header.Get("Cookie") != "" {
			return nil, fmt.Errorf("unexpected preview host or cookie")
		}
		status, mediaType, body := http.StatusNotFound, "text/plain", ""
		if req.Header.Get("Authorization") != "" && req.URL.Path != "/user-attachments/assets/unavailable" {
			t.Error("viewer credentials were reused for another attachment")
		}
		switch req.URL.Path {
		case "/user-attachments/assets/screenshot-id":
			status, mediaType, body = http.StatusOK, "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360"><rect width="640" height="360" fill="#246"/></svg>`
		case "/user-attachments/files/42/spec.pdf":
			status, mediaType, body = http.StatusOK, "application/octet-stream", taskAssetTestPDF()
		case "/user-attachments/files/42/notes.txt":
			status, mediaType, body = http.StatusOK, "text/plain", "<script>throw new Error('attachment executed')</script>\nUTF-8: café"
		case "/user-attachments/assets/unavailable":
			if req.Header.Get("Authorization") == "Bearer viewer-fixture" {
				status, body = http.StatusOK, "Private attachment preview"
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {mediaType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	run := issueRunConfig(GitHubRepo{FullName: "example/project"}, GitHubIssue{
		Number: 17, Title: "Inspect the attachments", State: "OPEN",
		Body:     "![Screenshot](https://github.com/user-attachments/assets/screenshot-id)",
		Comments: []GitHubIssueComment{{Author: GitHubIssueUser{Login: "reviewer"}, Body: "[Specification](https://github.com/user-attachments/files/42/spec.pdf)\n![Duplicate screenshot](https://github.com/user-attachments/assets/screenshot-id)"}},
	}, config.GitHubIssueLinkReference)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	srv.Broker.RecordTaskRunConfig("req-assets", raw)
	srv.Broker.RecordTaskRunConfig("req-single-asset", []byte(`{"repo":"example/project","prompt":"Task with one attachment","assets":[{"url":"https://github.com/user-attachments/assets/unavailable","name":"unavailable"}]}`))
	srv.Broker.RecordTaskRunConfig("req-text-asset", []byte(`{"repo":"example/project","prompt":"Task with a text attachment","assets":[{"url":"https://github.com/user-attachments/files/42/notes.txt","name":"notes.txt"}]}`))
	srv.Broker.RecordTaskRunConfig("req-no-assets", []byte(`{"repo":"example/project","prompt":"Task without attachments"}`))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runBrowserSuite(t, ts.URL, time.Minute, "task asset checks", "tests/task_assets.cjs")
}

func TestTaskAssetPreviewDoesNotUseServerAuthorization(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{Stdout: "fixture-token"}, nil
	}), true)
	srv.Broker.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[
		{"url":"https://github.com/user-attachments/assets/private"},
		{"url":"https://private-user-images.githubusercontent.com/42/private.png?jwt=fixture"},
		{"url":"https://github.com/user-attachments/assets/private?token=fixture"}
	]}`))
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/user-attachments/assets/private" || req.URL.RawQuery != "" {
			t.Error("preview fetched a stored capability URL")
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("preview forwarded authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("%PDF-1.4\nprivate bytes"))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
	})

	handler := srv.Handler()
	for index := range 3 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tasks/req-assets/asset-preview?index=%d", index), nil)
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private bytes") {
			t.Errorf("preview %d returned status %d, want 502 without attachment bytes", index, w.Code)
		}
	}
}

func TestTaskAssetPreviewUsesViewerAuthorization(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("preview looked up server GitHub credentials")
		return execx.Result{}, fmt.Errorf("unexpected GitHub CLI command")
	}), true)
	srv.Broker.RecordTaskRunConfig("req-private", []byte(`{"assets":[
		{"url":"https://github.com/user-attachments/assets/private"},
		{"url":"https://private-user-images.githubusercontent.com/42/private.png?jwt=stored-fixture"},
		{"url":"https://github.com/user-attachments/assets/private?token=stored-fixture"}
	]}`))
	const privateBody = "%PDF-1.4\nprivate bytes"
	var storageRequests int
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Cookie") != "" {
			t.Error("preview forwarded cookies")
		}
		switch req.URL.String() {
		case "https://github.com/user-attachments/assets/private":
			if req.Header.Get("Authorization") == "Bearer viewer-fixture" {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{
					"Location": {"https://private-user-images.githubusercontent.com/42/private.png?jwt=fresh-fixture"},
				}, Body: http.NoBody}, nil
			}
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		case "https://private-user-images.githubusercontent.com/42/private.png?jwt=fresh-fixture":
			storageRequests++
			if req.Header.Get("Authorization") != "" {
				t.Error("viewer token forwarded to storage")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(privateBody))}, nil
		default:
			t.Error("preview fetched an unexpected URL")
			return nil, fmt.Errorf("unexpected attachment request")
		}
	})
	handler := srv.Handler()
	for _, tc := range []struct {
		name, authorization string
		index, status       int
	}{
		{"other viewer", "Bearer denied-fixture", 0, http.StatusBadGateway},
		{"authorized viewer", "Bearer viewer-fixture", 0, http.StatusOK},
		{"anonymous after authorized", "", 0, http.StatusBadGateway},
		{"stored storage URL", "Bearer viewer-fixture", 1, http.StatusBadGateway},
		{"stored URL with query", "Bearer viewer-fixture", 2, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/tasks/req-private/asset-preview?index=%d", tc.index), nil)
			req.Header.Set("Authorization", tc.authorization)
			req.AddCookie(&http.Cookie{Name: "viewer", Value: "cookie-fixture"})
			handler.ServeHTTP(w, req)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("preview status = %d, cache control = %q, want %d and no-store", w.Code, w.Header().Get("Cache-Control"), tc.status)
			}
			if (w.Body.String() == privateBody) != (tc.status == http.StatusOK) {
				t.Fatal("unexpected private attachment access")
			}
		})
	}
	if storageRequests != 1 {
		t.Fatalf("storage requests = %d, want one viewer-authorized request", storageRequests)
	}
}

func taskAssetTestPDF() string {
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 240] /Contents 4 0 R >>",
		"<< /Length 29 >>\nstream\n0 0.3 0.5 rg 0 0 300 240 re f\nendstream",
	}
	var offsets []int
	for i, object := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return pdf.String()
}

func TestTaskAssetPreviewRequiresStoredAsset(t *testing.T) {
	t.Parallel()
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(context.Context, execx.Command) (execx.Result, error) {
		t.Error("unexpected credential lookup")
		return execx.Result{}, nil
	}), true)
	srv.Broker.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[{"url":"https://github.com/user-attachments/files/42/spec.pdf"}]}`))
	handler := srv.Handler()
	for _, path := range []string{
		"/api/tasks/missing/asset-preview?index=0",
		"/api/tasks/req-assets/asset-preview?index=-1",
		"/api/tasks/req-assets/asset-preview?index=1",
		"/api/tasks/req-assets/asset-preview?url=https://github.com/user-attachments/files/42/spec.pdf",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, w.Code)
		}
	}
}

func TestTaskAssetsSurviveSnapshotsWithoutSharingSlices(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	b.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Inspect files","assets":[{"url":"https://github.com/user-attachments/files/42/spec.pdf","name":"spec.pdf","source":"Comment 1"}]}`))
	snapshot := b.Snapshot()
	if len(snapshot.Tasks) != 1 || len(snapshot.Tasks[0].Assets) != 1 {
		t.Fatalf("missing assets: %+v", snapshot.Tasks)
	}
	snapshot.Tasks[0].Assets[0].Name = "changed"
	task, ok := b.Task("req-assets")
	if !ok || len(task.Assets) != 1 || task.Assets[0].Name != "spec.pdf" {
		t.Fatalf("snapshot mutated stored assets: %+v", task)
	}
	task.Assets[0].Name = "changed again"
	task, _ = b.Task("req-assets")
	if task.Assets[0].Name != "spec.pdf" {
		t.Fatal("Task returned mutable assets")
	}
	b.RecordTaskRunConfig("req-assets", []byte(`{"prompt":"Attachment removed"}`))
	task, _ = b.Task("req-assets")
	if len(task.Assets) != 0 {
		t.Fatal("removed assets persisted")
	}
}
