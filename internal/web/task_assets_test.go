package web

import "testing"

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
