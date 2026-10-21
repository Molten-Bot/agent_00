package web

import (
	"strings"
	"testing"
)

func TestBrokerNormalizesLegacyTaskLogs(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"task", "dispatch"} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			b := NewBroker()
			for _, step := range []struct{ line, status string }{
				{"status=start request_id=local-test", "running"},
				{"status=paused request_id=local-test", "paused"},
				{"status=resumed request_id=local-test", "pending"},
				{"status=start request_id=local-test", "running"},
				{"request_id=local-test stage=claude status=running elapsed_s=195", "running"},
				{"status=completed request_id=local-test", "completed"},
			} {
				b.IngestLog(prefix + " " + step.line)
				snap := b.Snapshot()
				if len(snap.Tasks) != 1 || snap.Tasks[0].Status != step.status {
					t.Fatalf("after %s: tasks=%+v", step.line, snap.Tasks)
				}
				for _, log := range snap.Tasks[0].Logs {
					if strings.HasPrefix(log.Text, "dispatch ") {
						t.Fatal("legacy name visible in task log")
					}
				}
			}
		})
	}
	for _, prefix := range []string{"scheduler", "dispatcher", "debug scheduler", "debug dispatcher"} {
		b := NewBroker()
		b.IngestLog(prefix + " status=window state=steady cpu=12.5 memory=48.2 disk_io_mb_s=3.7 allowed=2 max=4 running=1 queue_depth=0")
		if got := b.Snapshot().Resources.CPUPercent; got != 12.5 {
			t.Fatalf("%s cpu=%v", prefix, got)
		}
	}
}
