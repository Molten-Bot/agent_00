package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	srv := githubIssueTestServer(githubIssueRunnerFunc(func(_ context.Context, cmd execx.Command) (execx.Result, error) {
		if cmd.Name != "gh" || !slices.Equal(cmd.Args, []string{"auth", "token", "--hostname", "github.com"}) {
			return execx.Result{}, fmt.Errorf("unexpected GitHub CLI command")
		}
		return execx.Result{Stdout: "fixture-token\n"}, nil
	}), true)
	srv.assetTransport = taskAssetTransportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "github.com" || req.Header.Get("Authorization") != "Bearer fixture-token" {
			return nil, fmt.Errorf("missing GitHub CLI authentication")
		}
		status, mediaType, body := http.StatusNotFound, "text/plain", ""
		switch req.URL.Path {
		case "/user-attachments/assets/screenshot-id":
			status, mediaType, body = http.StatusOK, "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="360"><rect width="640" height="360" fill="#246"/></svg>`
		case "/user-attachments/files/42/spec.pdf":
			status, mediaType, body = http.StatusOK, "application/octet-stream", taskAssetTestPDF()
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
	srv.Broker.RecordTaskRunConfig("req-no-assets", []byte(`{"repo":"example/project","prompt":"Task without attachments"}`))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	runBrowserSuite(t, ts.URL, time.Minute, "task asset checks", "tests/task_assets.cjs")
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
