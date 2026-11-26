package localruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostMemoryPressurePausesAndRecovers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "meminfo")
	clock := time.Now()
	var logs []string
	check := memoryPressure{
		read: func() (linuxMemory, error) { return readLinuxMemory(path) },
		now:  func() time.Time { return clock },
		logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	sample := func(available, swapFree int) {
		t.Helper()
		fixture := fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\nSwapTotal: 4194304 kB\nSwapFree: %d kB\nShmem: 3072000 kB\n",
			15*1024*kibPerMiB, available*kibPerMiB, swapFree*kibPerMiB)
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Second)
	}
	sample(195, 512)
	if check.check() {
		t.Fatal("paused with available swap")
	}
	sample(195, 0)
	if !check.check() || !check.check() {
		t.Fatal("polling continued with 195 MiB available and full swap")
	}
	sample(600, 0)
	if !check.check() {
		t.Fatal("polling resumed below the recovery threshold")
	}
	sample(8192, 0)
	if check.check() {
		t.Fatal("full swap prevented recovery with ample available RAM")
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "status=warn state=paused mem_available_mib=195 mem_total_mib=15360 swap_free_mib=0 swap_total_mib=4096 shmem_mib=3000") ||
		!strings.Contains(logs[1], "status=ok state=resumed") {
		t.Fatalf("pressure transitions = %v", logs)
	}
}

func TestSchedulerHoldsSingleWorkerAdmissionDuringMemoryPressure(t *testing.T) {
	t.Parallel()
	var paused atomic.Bool
	scheduler := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, t.Logf, WithSchedulerMemoryPressure(paused.Load))
	scheduler.sample = staticSample{}
	release, err := scheduler.Acquire(t.Context(), "active")
	if err != nil {
		t.Fatal(err)
	}
	paused.Store(true)
	done := make(chan error, 1)
	go func() {
		release, err := scheduler.Acquire(t.Context(), "waiting")
		if err == nil {
			release()
		}
		done <- err
	}()
	waitForQueuedRequest(t, scheduler, "waiting")
	release()
	scheduler.mu.Lock()
	queued := len(scheduler.waiters)
	scheduler.mu.Unlock()
	if queued != 1 {
		t.Fatal("released worker slot admitted work during pressure")
	}
	paused.Store(false)
	scheduler.sampleAndUpdate()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued work did not resume")
	}
}
