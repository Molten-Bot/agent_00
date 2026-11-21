package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func TestTaskAssetsRenderInBrowser(t *testing.T) {
	requireBrowserTooling(t, "task asset checks")
	srv := githubIssueTestServer(nil, true)
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
