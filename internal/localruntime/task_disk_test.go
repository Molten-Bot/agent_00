package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestTaskDiskHistoryUsesRecentMeasurements(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "task-disk-usage.json")
	history, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	key := TaskDiskKey([]string{"https://github.com/example/one.git", "https://github.com/example/two"})
	alias := TaskDiskKey([]string{"git@github.com:example/two.git", "ssh://git@github.com/Example/One"})
	if key != alias || key == TaskDiskKey([]string{"https://github.com/example/two"}) {
		t.Fatal("repository hash did not identify the repository combination")
	}

	var writes sync.WaitGroup
	for _, size := range []uint64{64, 256, 128} {
		writes.Go(func() {
			if err := history.Record(key, size); err != nil {
				t.Error(err)
			}
		})
	}
	writes.Wait()
	if got := history.RequiredBytes(alias); got != 256 {
		t.Fatalf("disk requirement after concurrent writes = %d, want 256", got)
	}
	record := func(size, want uint64) {
		t.Helper()
		if err := history.Record(key, size); err != nil {
			t.Fatal(err)
		}
		if got := history.RequiredBytes(alias); got != want {
			t.Fatalf("disk requirement after recording %d = %d, want %d", size, got, want)
		}
	}
	record(1024, 1024)
	for range taskDiskHistoryWindow - 1 {
		record(64, 1024)
	}

	history, err = OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	record(64, 64)
	record(256, 256)
	record(512, 512)
	record(512, 512)
	record(64, 512)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "example") {
		t.Fatal("history contains a raw repository reference")
	}
}

func TestTaskDiskHistoryFlushRetriesFailedWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "disk-history.json")
	history, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	key := TaskDiskKey([]string{"https://github.com/example/repo"})
	// A directory at the destination prevents the atomic replacement.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := history.Record(key, 256); err == nil {
		t.Fatal("record succeeded while destination was blocked")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := history.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RequiredBytes(key); got != 256 {
		t.Fatalf("reopened disk requirement = %d, want 256", got)
	}
}

func TestTaskDiskHistoryFlushRetriesVersionedWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "task-disk-usage.json")
	history, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	versioned := TaskDiskSamplesPath(path)
	if err := os.Mkdir(versioned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := history.Record("repo", 256); err == nil {
		t.Fatal("record succeeded while versioned destination was blocked")
	}
	fallback, err := OpenTaskDiskHistory(path)
	if err == nil {
		t.Error("open did not report the unreadable versioned history")
	}
	if got := fallback.RequiredBytes("repo"); got != 256 {
		t.Errorf("requirement while versioned history is unreadable = %d, want 256", got)
	}
	if err := os.Remove(versioned); err != nil {
		t.Fatal(err)
	}
	if err := history.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * taskDiskLegacyMaxAge)
	reopened.now = func() time.Time { return now }
	if got := reopened.RequiredBytes("repo"); got != 256 {
		t.Fatalf("timestamped sample was not recovered: got %d, want 256", got)
	}
}

func TestTaskDiskHistoryMergesPendingSamplesAfterFailedLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "disk-history.json")
	invalid := []byte(`{"first":256,"second":"invalid"}`)
	if err := os.WriteFile(path, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	history, err := OpenTaskDiskHistory(path)
	if err == nil {
		t.Fatal("invalid history loaded successfully")
	}
	for _, size := range []uint64{128, 64, 64} {
		if err := history.Record("first", size); err == nil {
			t.Fatal("record succeeded while history could not be loaded")
		}
	}
	if err := history.Record("third", 512); err == nil {
		t.Fatal("record succeeded while history could not be loaded")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(invalid) {
		t.Fatalf("unreadable history changed: %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"first":256,"second":[64,32]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := history.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]uint64{"first": 256, "second": 64, "third": 512} {
		if got := reopened.RequiredBytes(key); got != want {
			t.Errorf("required bytes for %q = %d, want %d", key, got, want)
		}
	}
	if err := reopened.Record("first", 64); err != nil {
		t.Fatal(err)
	}
	if got := reopened.RequiredBytes("first"); got != 128 {
		t.Fatalf("estimate after aging out the legacy sample = %d, want 128", got)
	}
	now := time.Now().Add(taskDiskLegacyMaxAge)
	reopened.now = func() time.Time { return now }
	if got := reopened.RequiredBytes("second"); got != 0 {
		t.Fatalf("estimate after both imported samples expire = %d, want 0", got)
	}
}

func TestTaskDiskHistoryRemainsWritableAfterRollback(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "task-disk-usage.json")
	if err := os.WriteFile(path, []byte(`{"existing":256}`), 0o600); err != nil {
		t.Fatal(err)
	}
	history, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Record("new", 128); err != nil {
		t.Fatal(err)
	}

	// Use the previous release's scalar decoder and high-water update.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var maxima map[string]uint64
	if err := json.Unmarshal(data, &maxima); err != nil {
		t.Fatalf("rollback cannot read history: %v", err)
	}
	if maxima["existing"] != 256 || maxima["new"] != 128 {
		t.Fatalf("rollback estimates = %v", maxima)
	}
	maxima["new"] = 512
	data, err = json.Marshal(maxima)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRuntimeConfigFile(path, data); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(TaskDiskSamplesPath(path))
	if err != nil {
		t.Fatal(err)
	}
	rollbackAt := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, rollbackAt, rollbackAt); err != nil {
		t.Fatal(err)
	}
	history, err = OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := history.RequiredBytes("new"); got != 512 {
		t.Fatalf("estimate after re-upgrade = %d, want 512", got)
	}
	history.now = func() time.Time { return rollbackAt.Add(taskDiskLegacyMaxAge) }
	if got := history.RequiredBytes("new"); got != 128 {
		t.Fatalf("estimate after rollback sample expires = %d, want 128", got)
	}
}

func TestTaskDiskHistoryRetainsBaselineAfterIdle(t *testing.T) {
	t.Parallel()

	const baseline = 2 * 1024 * 1024 * 1024
	path := filepath.Join(t.TempDir(), "task-disk-usage.json")
	history, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := time.Now().Add(-taskDiskSampleMaxAge - time.Minute)
	history.now = func() time.Time { return recordedAt }
	for _, size := range []uint64{baseline, baseline, 3 * baseline, baseline} {
		if err := history.Record("repo", size); err != nil {
			t.Fatal(err)
		}
	}
	if got := history.RequiredBytes("repo"); got != 3*baseline {
		t.Fatalf("fresh estimate = %d, want %d", got, 3*baseline)
	}
	history.now = func() time.Time { return recordedAt.Add(taskDiskSampleMaxAge) }
	if got := history.RequiredBytes("repo"); got != baseline {
		t.Errorf("idle estimate = %d, want %d", got, baseline)
	}
	reopened, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RequiredBytes("repo"); got != baseline {
		t.Errorf("idle estimate after restart = %d, want %d", got, baseline)
	}
	if err := reopened.Record("other-repo", 128); err != nil {
		t.Fatal(err)
	}
	reopened, err = OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RequiredBytes("repo"); got != baseline {
		t.Errorf("idle estimate after resaving history = %d, want %d", got, baseline)
	}
	for range taskDiskHistoryWindow {
		if err := reopened.Record("repo", baseline/2); err != nil {
			t.Fatal(err)
		}
	}
	if got := reopened.RequiredBytes("repo"); got != baseline/2 {
		t.Errorf("estimate after smaller runs = %d, want %d", got, baseline/2)
	}
}

func TestAdaptiveSchedulerAdmitsAfterDiskOutlierExpires(t *testing.T) {
	t.Parallel()

	const gib = 1024 * 1024 * 1024
	const peak uint64 = 5509423104
	for _, tc := range []struct {
		name  string
		total uint64
		issue bool
	}{
		{name: "migrated", total: 8 * gib},
		{name: "measured", total: 8 * gib},
		{name: "issue on smaller volume", total: 4 * gib, issue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "task-disk-usage.json")
			now := time.Now().UTC().Truncate(time.Second)
			expiresAt := now.Add(time.Minute)
			if tc.name == "migrated" {
				if err := os.WriteFile(path, fmt.Appendf(nil, `{"repo":%d}`, peak), 0o600); err != nil {
					t.Fatal(err)
				}
				legacyAt := expiresAt.Add(-taskDiskLegacyMaxAge)
				if err := os.Chtimes(path, legacyAt, legacyAt); err != nil {
					t.Fatal(err)
				}
			}
			history, err := OpenTaskDiskHistory(path)
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(0)
			if tc.name != "migrated" {
				history.now = func() time.Time { return expiresAt.Add(-taskDiskSampleMaxAge) }
				if err := history.Record("repo", peak); err != nil {
					t.Fatal(err)
				}
				history.now = func() time.Time { return now }
				want = 2 * gib
				if err := history.Record("repo", want); err != nil {
					t.Fatal(err)
				}
				history, err = OpenTaskDiskHistory(path)
				if err != nil {
					t.Fatal(err)
				}
			}

			history.now = func() time.Time { return now }
			controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
			controller.diskSpace = func(string) (string, uint64, uint64, error) {
				return "disk", 4 * gib, tc.total, nil
			}
			request := TaskDiskRequest{
				Workspace: workspace.Manager{Root: t.TempDir()},
				RequiredBytes: func() uint64 {
					if size := history.RequiredBytes("repo"); size > 0 {
						return size
					}
					return workspace.MinimumFreeBytes
				},
			}
			if tc.issue {
				request.CheckPersistentStorage = func(string) error { return nil }
			}
			waiter := &taskWaiter{disk: &request}
			controller.waiters = []*taskWaiter{waiter}
			controller.RecheckQueue()
			if waiter.granted || waiter.err != nil || waiter.diskEstimate != peak || waiter.diskBlock == "" || len(controller.waiters) != 1 {
				t.Fatalf("outlier did not keep task queued: granted=%t err=%v estimate=%d block=%q waiters=%d",
					waiter.granted, waiter.err, waiter.diskEstimate, waiter.diskBlock, len(controller.waiters))
			}
			now = expiresAt
			controller.RecheckQueue()
			if got := history.RequiredBytes("repo"); got != want {
				t.Errorf("estimate after expiry = %d, want %d", got, want)
			}
			if !waiter.granted || waiter.diskBytes != max(want, workspace.MinimumFreeBytes) {
				t.Fatalf("admission after expiry: %+v", waiter)
			}
			controller.releaseFunc(waiter)()
		})
	}
}

func TestAdaptiveSchedulerRechecksRollingEstimateWithActiveReservation(t *testing.T) {
	t.Parallel()

	const floor = workspace.MinimumFreeBytes
	history, err := OpenTaskDiskHistory("")
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Record("repo", 3*floor); err != nil {
		t.Fatal(err)
	}
	var logs []string
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 2}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	free := uint64(3 * floor)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", free, math.MaxUint64, nil
	}
	request := TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	_, release, err := controller.AcquireWorkspace(t.Context(), "active", request, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	request.RequiredBytes = func() uint64 { return history.RequiredBytes("repo") }
	queued := &taskWaiter{requestID: "queued", disk: &request, queued: true}
	controller.waiters = []*taskWaiter{queued}
	controller.RecheckQueue()
	if queued.granted || !strings.Contains(queued.diskBlock, fmt.Sprintf("estimated_bytes=%d", 3*floor)) {
		t.Fatalf("initial admission granted = %t, disk block = %q", queued.granted, queued.diskBlock)
	}
	for range taskDiskHistoryWindow {
		if err := history.Record("repo", floor/2); err != nil {
			t.Fatal(err)
		}
	}
	free = 2*floor - 1
	controller.RecheckQueue()
	if queued.granted {
		t.Fatal("admission consumed the free-space floor after accounting for the active reservation")
	}
	updated := fmt.Sprintf(`required_bytes=%d reserved_bytes=%d blocking_request_ids="active" estimated_bytes=%d minimum_free_bytes=%d`, floor, floor, floor/2, floor)
	if len(logs) != 2 || !strings.Contains(logs[1], "reason=disk_space ") || !strings.Contains(logs[1], updated) {
		t.Fatalf("missing updated queue estimate and reservation in logs: %v", logs)
	}
	free = 2 * floor
	controller.RecheckQueue()
	if !queued.granted {
		t.Fatalf("admission remained blocked after space reached the floor: %s", queued.diskBlock)
	}
	controller.releaseFunc(queued)()
}

func TestAdaptiveSchedulerReservesSpacePerFilesystem(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	controller.sample = staticSample{}
	root, sameVolume, other := t.TempDir(), t.TempDir(), t.TempDir()
	const unit = workspace.MinimumFreeBytes
	controller.diskSpace = func(path string) (string, uint64, uint64, error) {
		if path == other {
			return "other-volume", 3 * unit, math.MaxUint64, nil
		}
		return "shared-volume", 3 * unit, math.MaxUint64, nil
	}
	request := TaskDiskRequest{
		Workspace:     workspace.Manager{Root: root},
		RequiredBytes: func() uint64 { return 2 * unit },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gotRoot, releaseFirst, err := controller.AcquireWorkspace(ctx, "first", request, false)
	if err != nil || gotRoot != root {
		t.Fatalf("first acquisition root = %q, error = %v", gotRoot, err)
	}
	defer releaseFirst()

	second := make(chan error, 1)
	secondRequest := request
	secondRequest.Workspace.Root = sameVolume
	go func() {
		_, release, err := controller.AcquireWorkspace(ctx, "second", secondRequest, false)
		if err == nil {
			release()
		}
		second <- err
	}()
	waitForQueuedRequest(t, controller, "second")

	otherRequest := request
	otherRequest.Workspace.Root = other
	gotRoot, releaseOther, err := controller.AcquireWorkspace(ctx, "other", otherRequest, false)
	if err != nil || gotRoot != other {
		t.Fatalf("other acquisition root = %q, error = %v", gotRoot, err)
	}
	defer releaseOther()
	select {
	case err := <-second:
		t.Fatalf("second acquisition returned before disk reservation was released: %v", err)
	default:
	}
	releaseFirst()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveSchedulerSelectsFallbackWorkspaceWithRoom(t *testing.T) {
	ram, disk := t.TempDir(), t.TempDir()
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", ram)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", disk)
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "test-workspaces")

	targets := workspace.Manager{
		PathExists: func(string) bool { return true },
		CanExec:    func(string) bool { return true },
	}
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.diskSpace = func(path string) (string, uint64, uint64, error) {
		if filepath.Dir(path) == ram {
			return ram, workspace.MinimumFreeBytes, math.MaxUint64, nil
		}
		return disk, 3 * workspace.MinimumFreeBytes, math.MaxUint64, nil
	}
	request := TaskDiskRequest{Workspace: targets, RequiredBytes: func() uint64 {
		return 2 * workspace.MinimumFreeBytes
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	root, release, err := controller.AcquireWorkspace(ctx, "fallback", request, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if want := filepath.Join(disk, "test-workspaces"); root != want {
		t.Fatalf("workspace root = %q, want %q", root, want)
	}
}

func TestAdaptiveSchedulerRetriesDiskPressureWithFailedCPUSample(t *testing.T) {
	t.Parallel()

	const required = 2 * workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.sample = staticSample{err: errors.New("metrics unavailable")}
	root := t.TempDir()
	free := uint64(required - 1)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", free, math.MaxUint64, nil
	}
	request := TaskDiskRequest{
		Workspace:     workspace.Manager{Root: root},
		RequiredBytes: func() uint64 { return required },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, release, err := controller.AcquireWorkspace(ctx, "waiting", request, false)
		if err == nil {
			release()
		}
		done <- err
	}()
	waitForQueuedRequest(t, controller, "waiting")
	controller.mu.Lock()
	free = required
	controller.mu.Unlock()

	controller.sampleAndUpdate()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveSchedulerRechecksQueueAfterWorkspacePurge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	retained := filepath.Join(root, "completed-task")
	if err := os.Mkdir(retained, 0700); err != nil {
		t.Fatal(err)
	}
	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 2, SampleWindow: 1}, nil)
	controller.sample = staticSample{value: resourceSample{CPUPercent: 95}}
	controller.sampleAndUpdate()
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		_, err := os.Stat(retained)
		if errors.Is(err, os.ErrNotExist) {
			return "disk", 2 * unit, 2 * unit, nil
		}
		return "disk", 0, 2 * unit, err
	}
	request := TaskDiskRequest{
		Workspace:     workspace.Manager{Root: root},
		RequiredBytes: func() uint64 { return unit },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type admission struct {
		release func()
		err     error
	}
	queue := func(id string) <-chan admission {
		done := make(chan admission, 1)
		go func() {
			_, release, err := controller.AcquireWorkspace(ctx, id, request, false)
			done <- admission{release: release, err: err}
		}()
		waitForQueuedRequest(t, controller, id)
		return done
	}
	first, second := queue("first"), queue("second")
	if err := os.RemoveAll(retained); err != nil {
		t.Fatal(err)
	}
	controller.RecheckQueue()
	got := <-first
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.release()
	waitForQueuedRequest(t, controller, "second")

	controller.sample = staticSample{value: resourceSample{CPUPercent: 20}}
	controller.sampleAndUpdate()
	got = <-second
	if got.err != nil {
		t.Fatal(got.err)
	}
	got.release()
}

func TestAdaptiveSchedulerCanForceStartEstimateLargerThanFilesystem(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", unit, unit, nil
	}
	root := t.TempDir()
	request := TaskDiskRequest{
		Workspace:     workspace.Manager{Root: root},
		RequiredBytes: func() uint64 { return 2 * unit },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queuedCtx, cancelQueued := context.WithCancel(ctx)
	defer cancelQueued()
	done := make(chan error, 1)
	go func() {
		_, release, err := controller.AcquireWorkspace(queuedCtx, "oversized", request, false)
		if release != nil {
			release()
		}
		done <- err
	}()
	waitForQueuedRequest(t, controller, "oversized")

	// Force start cancels the pending acquire before retrying with the override.
	cancelQueued()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued acquisition error = %v, want cancellation", err)
	}
	gotRoot, release, err := controller.AcquireWorkspace(ctx, "oversized", request, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if gotRoot != root {
		t.Fatalf("forced workspace root = %q, want %q", gotRoot, root)
	}
}

func TestAdaptiveSchedulerReportsFilesystemSmallerThanWorkspaceMinimum(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", unit - 1, unit - 1, nil
	}
	request := TaskDiskRequest{
		Workspace: workspace.Manager{Root: t.TempDir(), CheckSpace: func(string) error { return syscall.ENOSPC }},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := controller.AcquireWorkspace(ctx, "oversized", request, false)
	if release != nil {
		release()
		t.Fatal("admitted task on a filesystem smaller than the workspace minimum")
	}
	if err == nil || !strings.Contains(err.Error(), "exceeding total filesystem capacity") {
		t.Fatalf("acquisition error = %v, want filesystem capacity error", err)
	}
}

func TestAdaptiveSchedulerReportsWorkspaceSetupFailure(t *testing.T) {
	t.Parallel()

	for _, force := range []bool{false, true} {
		controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
		controller.diskSpace = func(string) (string, uint64, uint64, error) {
			return "disk", 0, math.MaxUint64, nil
		}
		request := TaskDiskRequest{Workspace: workspace.Manager{
			Root: t.TempDir(),
			MkdirAll: func(string, os.FileMode) error {
				return os.ErrPermission
			},
		}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, release, err := controller.AcquireWorkspace(ctx, "broken-root", request, force)
		cancel()
		if release != nil || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("force=%v: acquisition error = %v, want %v", force, err, os.ErrPermission)
		}
	}
}

func TestAdaptiveSchedulerQueuesWhenWorkspaceRootIsOutOfSpace(t *testing.T) {
	t.Parallel()
	testAdaptiveSchedulerQueuesWhenWorkspaceRootIsOutOfSpace(t, syscall.ENOSPC)
}

func testAdaptiveSchedulerQueuesWhenWorkspaceRootIsOutOfSpace(t *testing.T, diskFull error) {
	t.Helper()
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.sample = staticSample{}
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", workspace.MinimumFreeBytes, math.MaxUint64, nil
	}
	root := filepath.Join(t.TempDir(), "workspaces")
	full := true
	request := TaskDiskRequest{Workspace: workspace.Manager{
		Root: root,
		MkdirAll: func(path string, mode os.FileMode) error {
			if full {
				return &os.PathError{Op: "mkdir", Path: path, Err: diskFull}
			}
			return os.MkdirAll(path, mode)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, release, err := controller.AcquireWorkspace(ctx, "full-root", request, false)
		if err == nil {
			release()
		}
		done <- err
	}()
	waitForQueuedRequest(t, controller, "full-root")
	controller.mu.Lock()
	full = false
	controller.mu.Unlock()
	controller.sampleAndUpdate()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveSchedulerDrainsReservationsForOlderDiskWaiter(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 4}, nil)
	sameDevice, otherDevice := t.TempDir(), t.TempDir()
	controller.diskSpace = func(path string) (string, uint64, uint64, error) {
		if path == otherDevice {
			return "other", 3 * unit, math.MaxUint64, nil
		}
		return "shared", 3 * unit, math.MaxUint64, nil
	}

	small := TaskDiskRequest{Workspace: workspace.Manager{Root: sameDevice}}
	large := small
	large.Workspace.Root = t.TempDir()
	large.RequiredBytes = func() uint64 { return 3 * unit }
	unrelated := TaskDiskRequest{Workspace: workspace.Manager{Root: otherDevice}}
	running := &taskWaiter{disk: &small, requestID: "active-task"}
	older := &taskWaiter{disk: &large}
	controller.waiters = []*taskWaiter{running, older}
	controller.tryGrantLocked(0)
	if !running.granted || older.diskBlock == "" {
		t.Fatalf("running admitted = %v, older disk blocked = %v", running.granted, older.diskBlock != "")
	}
	if !strings.Contains(older.diskBlock, `reserved_bytes=536870912 blocking_request_ids="active-task"`) {
		t.Fatalf("queue reason does not identify the active reservation: %s", older.diskBlock)
	}

	var younger []*taskWaiter
	for range 3 {
		waiter := &taskWaiter{disk: &small}
		younger = append(younger, waiter)
		controller.waiters = append(controller.waiters, waiter)
		controller.tryGrantLocked(len(controller.waiters) - 1)
		if waiter.granted || waiter.diskBlock == "" {
			t.Fatal("younger job was not queued behind the older job on shared storage")
		}
	}
	other := &taskWaiter{disk: &unrelated}
	controller.waiters = append(controller.waiters, other)
	controller.tryGrantLocked(len(controller.waiters) - 1)
	if !other.granted {
		t.Fatal("job on unrelated storage was not admitted")
	}

	controller.releaseFunc(running)()
	if !older.granted {
		t.Fatal("older job was not admitted after the running reservation released")
	}
	for _, waiter := range younger {
		if waiter.granted {
			t.Fatal("younger job was admitted while the older job reserved shared storage")
		}
	}
	controller.releaseFunc(older)()
	for _, waiter := range younger {
		if !waiter.granted {
			t.Fatal("younger job was not admitted after the older job released shared storage")
		}
	}
}

func TestAdaptiveSchedulerDrainsWorkspacesForOlderDiskWaiter(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	for _, tc := range []struct {
		name        string
		cleanup     bool
		usage       uint64
		reservation uint64
		required    uint64
		total       uint64
		wantDrain   bool
	}{
		{name: "within reservation", cleanup: true, usage: unit, reservation: unit, required: 4 * unit, total: 4 * unit, wantDrain: true},
		{name: "over reservation", cleanup: true, usage: 2 * unit, reservation: unit, required: 4 * unit, total: 4 * unit, wantDrain: true},
		{name: "without reserved bytes", cleanup: true, usage: 2 * unit, required: 4 * unit, total: 4 * unit, wantDrain: true},
		{name: "oversized after cleanup", cleanup: true, usage: 2 * unit, reservation: unit, required: 5 * unit, total: 6 * unit, wantDrain: true},
		{name: "exceeds filesystem capacity", cleanup: true, usage: 2 * unit, reservation: unit, required: 5 * unit, total: 4 * unit},
		{name: "no cleanup", usage: unit, reservation: unit, required: 4 * unit, total: 4 * unit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
			free := uint64(4 * unit)
			controller.diskSpace = func(string) (string, uint64, uint64, error) {
				return "shared", free, tc.total, nil
			}
			small := TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
			active := small
			active.CleanupOnCompletion = tc.cleanup
			active.RequiredBytes = func() uint64 { return tc.reservation }
			running := &taskWaiter{disk: &active}
			controller.waiters = []*taskWaiter{running}
			controller.tryGrantLocked(0)
			if !running.granted {
				t.Fatal("initial task was not admitted")
			}

			free -= tc.usage
			large := small
			large.RequiredBytes = func() uint64 { return tc.required }
			older := &taskWaiter{disk: &large}
			controller.waiters = []*taskWaiter{older}
			controller.tryGrantLocked(0)
			if older.granted {
				t.Fatal("older task admitted before storage was available")
			}
			younger := &taskWaiter{disk: &small}
			controller.waiters = append(controller.waiters, younger)
			controller.tryGrantLocked(len(controller.waiters) - 1)
			if younger.granted == tc.wantDrain {
				t.Fatalf("younger admitted = %v, want drain = %v", younger.granted, tc.wantDrain)
			}

			if tc.cleanup {
				free += tc.usage
			}
			controller.releaseFunc(running)()
			if older.granted != (free >= tc.required) {
				t.Fatalf("older admitted = %v after release with %d bytes free, requires %d", older.granted, free, tc.required)
			}
			if !older.granted && !younger.granted {
				t.Fatal("younger job stayed queued after the draining workspace released")
			}
		})
	}
}

func TestAdaptiveSchedulerResumesAdmissionWhenDrainingWaiterCancels(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "shared", 3 * unit, 3 * unit, nil
	}
	small := TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	large := small
	large.RequiredBytes = func() uint64 { return 3 * unit }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, release, err := controller.AcquireWorkspace(ctx, "running", small, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	olderCtx, cancelOlder := context.WithCancel(ctx)
	defer cancelOlder()
	olderDone := make(chan error, 1)
	go func() {
		_, release, err := controller.AcquireWorkspace(olderCtx, "older", large, false)
		if release != nil {
			release()
		}
		olderDone <- err
	}()
	waitForQueuedRequest(t, controller, "older")
	youngerDone := make(chan error, 1)
	go func() {
		_, release, err := controller.AcquireWorkspace(ctx, "younger", small, false)
		if release != nil {
			release()
		}
		youngerDone <- err
	}()
	waitForQueuedRequest(t, controller, "younger")

	cancelOlder()
	if err := <-olderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("older acquisition error = %v, want cancellation", err)
	}
	if err := <-youngerDone; err != nil {
		t.Fatalf("younger acquisition after drain canceled: %v", err)
	}
}

func TestAdaptiveSchedulerAdmitsSmallerTaskBehindOversizedWaiter(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "shared", 2 * unit, math.MaxUint64, nil
	}
	small := TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	large := small
	large.RequiredBytes = func() uint64 { return 3 * unit }
	running := &taskWaiter{disk: &small}
	older := &taskWaiter{disk: &large}
	controller.waiters = []*taskWaiter{running, older}
	controller.tryGrantLocked(0)
	if !running.granted || older.diskBlock == "" {
		t.Fatalf("running admitted = %v, oversized task disk blocked = %v", running.granted, older.diskBlock != "")
	}
	younger := &taskWaiter{disk: &small}
	controller.waiters = append(controller.waiters, younger)
	controller.tryGrantLocked(len(controller.waiters) - 1)
	if !younger.granted {
		t.Fatal("oversized task blocked a younger job that fits on shared storage")
	}
}

func TestAdaptiveSchedulerWaitsBeforeReadmittingFailedAllocation(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.sample = staticSample{}
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", workspace.MinimumFreeBytes, math.MaxUint64, nil
	}
	root := t.TempDir()
	request := TaskDiskRequest{
		Workspace:  workspace.Manager{Root: root},
		RetryRoot:  root,
		RetryAfter: time.Now().Add(time.Hour),
	}
	waiter := &taskWaiter{disk: &request}
	controller.waiters = []*taskWaiter{waiter}
	controller.tryGrantLocked(0)
	if waiter.granted || waiter.diskBlock == "" {
		t.Fatal("disk-full retry was not queued while waiting to retry")
	}
	request.RetryAfter = time.Now().Add(-time.Second)
	controller.sampleAndUpdate()
	if !waiter.granted || waiter.root != root {
		t.Fatalf("retry admitted = %v, root = %q, want %q", waiter.granted, waiter.root, root)
	}
}

func TestAdaptiveSchedulerLogsDiskSpaceAfterAllocationCooldown(t *testing.T) {
	t.Parallel()

	const floor = workspace.MinimumFreeBytes
	var logs []string
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 2}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", floor, math.MaxUint64, nil
	}
	request := TaskDiskRequest{Workspace: workspace.Manager{Root: t.TempDir()}}
	_, release, err := controller.AcquireWorkspace(t.Context(), "active", request, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	request.RetryAfter = time.Now().Add(time.Hour)
	waiter := &taskWaiter{requestID: "retry", disk: &request, queued: true}
	controller.waiters = []*taskWaiter{waiter}
	controller.RecheckQueue()
	request.RetryAfter = time.Now().Add(-time.Second)
	controller.RecheckQueue()
	controller.RecheckQueue()

	if waiter.granted || len(logs) != 2 || !strings.Contains(logs[0], "retry_at=") {
		t.Fatalf("expected cooldown followed by disk block: granted=%t logs=%v", waiter.granted, logs)
	}
	details := fmt.Sprintf(`free_bytes=%d required_bytes=%d reserved_bytes=%d blocking_request_ids="active" estimated_bytes=%d`, floor, floor, floor, floor)
	if strings.Contains(logs[1], "retry_at=") || !strings.Contains(logs[1], details) {
		t.Fatalf("expected current disk space and reservations: %s", logs[1])
	}
}

func TestAdaptiveSchedulerTriesAnotherWorkspaceAfterSetupFailure(t *testing.T) {
	ram, disk := t.TempDir(), t.TempDir()
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", ram)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", disk)
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "test-workspaces")

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.diskSpace = func(path string) (string, uint64, uint64, error) {
		return filepath.Dir(path), workspace.MinimumFreeBytes, math.MaxUint64, nil
	}
	request := TaskDiskRequest{Workspace: workspace.Manager{
		PathExists: func(string) bool { return true },
		CanExec:    func(string) bool { return true },
		MkdirAll: func(path string, mode os.FileMode) error {
			if filepath.Dir(path) == ram {
				return os.ErrPermission
			}
			return os.MkdirAll(path, mode)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	root, release, err := controller.AcquireWorkspace(ctx, "fallback", request, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if want := filepath.Join(disk, "test-workspaces"); root != want {
		t.Fatalf("workspace root = %q, want %q", root, want)
	}
}

func TestAdaptiveSchedulerKeepsOversizedPreferredRootForceStartable(t *testing.T) {
	const unit = workspace.MinimumFreeBytes
	ram, disk := t.TempDir(), t.TempDir()
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", ram)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", disk)
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "test-workspaces")

	request := TaskDiskRequest{
		Workspace: workspace.Manager{
			PathExists: func(string) bool { return true },
			CanExec:    func(string) bool { return true },
			CheckSpace: func(string) error { return nil },
			MkdirAll: func(path string, _ os.FileMode) error {
				if filepath.Dir(path) == disk {
					return os.ErrPermission
				}
				return nil
			},
		},
		RequiredBytes: func() uint64 { return 2 * unit },
	}
	for _, total := range []uint64{unit - 1, unit, 2 * unit} {
		controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
		controller.diskSpace = func(string) (string, uint64, uint64, error) {
			return "ram", min(unit, total), total, nil
		}
		waiter := &taskWaiter{disk: &request}
		controller.waiters = []*taskWaiter{waiter}
		controller.tryGrantLocked(0)
		if waiter.err != nil || waiter.diskBlock == "" || waiter.granted {
			t.Fatalf("capacity %d: error = %v, blocked = %v, granted = %v", total, waiter.err, waiter.diskBlock != "", waiter.granted)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		root, release, err := controller.AcquireWorkspace(ctx, "override", request, true)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		release()
		if want := filepath.Join(ram, "test-workspaces"); root != want {
			t.Fatalf("capacity %d: forced root = %q, want %q", total, root, want)
		}
	}
}

func TestAdaptiveSchedulerReservesFullForcedEstimate(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	free := uint64(workspace.MinimumFreeBytes)
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		return "disk", free, math.MaxUint64, nil
	}
	request := TaskDiskRequest{
		Workspace:     workspace.Manager{Root: t.TempDir()},
		RequiredBytes: func() uint64 { return math.MaxUint64 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, releaseFirst, err := controller.AcquireWorkspace(ctx, "forced-first", request, true)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	_, releaseSecond, err := controller.AcquireWorkspace(ctx, "forced-second", request, true)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	free = math.MaxUint64

	done := make(chan error, 1)
	request.RequiredBytes = nil
	go func() {
		_, release, err := controller.AcquireWorkspace(ctx, "normal", request, false)
		if err == nil {
			release()
		}
		done <- err
	}()
	waitForQueuedRequest(t, controller, "normal")
	releaseFirst()
	controller.mu.Lock()
	remaining := len(controller.waiters)
	controller.mu.Unlock()
	if remaining != 1 {
		t.Fatal("normal task admitted with a forced reservation still running")
	}
	releaseSecond()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveSchedulerDoesNotReprobeDiskWaitersOnSubmission(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.sample = staticSample{}
	root := t.TempDir()
	probes := 0
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		probes++
		return "disk", 0, math.MaxUint64, nil
	}
	request := TaskDiskRequest{Workspace: workspace.Manager{Root: root}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const submissions = 12
	done := make(chan error, submissions)
	for i := range submissions {
		id := strconv.Itoa(i)
		go func() {
			_, _, err := controller.AcquireWorkspace(ctx, id, request, false)
			done <- err
		}()
		waitForQueuedRequest(t, controller, id)
	}
	cancel()
	for range submissions {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("acquisition error = %v, want cancellation", err)
		}
	}
	if probes != submissions {
		t.Fatalf("disk probes = %d, want %d", probes, submissions)
	}
}

func TestAdaptiveSchedulerSharesDiskSamplesWithinAdmissionPass(t *testing.T) {
	t.Parallel()

	const unit = workspace.MinimumFreeBytes
	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	preparations := 0
	request := TaskDiskRequest{Workspace: workspace.Manager{
		Root: t.TempDir(),
		MkdirAll: func(path string, mode os.FileMode) error {
			preparations++
			return os.MkdirAll(path, mode)
		},
	}}
	first := &taskWaiter{disk: &request}
	second := &taskWaiter{disk: &request}
	third := &taskWaiter{disk: &request}
	controller.waiters = []*taskWaiter{first, second, third}
	free := uint64(2 * unit)
	probes := 0
	controller.diskSpace = func(string) (string, uint64, uint64, error) {
		probes++
		return "disk", free, math.MaxUint64, nil
	}

	controller.tryGrantLocked(0)
	if preparations != 1 {
		t.Fatalf("first pass: root preparations = %d, want 1", preparations)
	}
	if probes != 1 || !first.granted || !second.granted || third.diskBlock == "" || third.granted {
		t.Fatalf("first pass: probes = %d, admitted = %v/%v/%v, third disk blocked = %v",
			probes, first.granted, second.granted, third.granted, third.diskBlock != "")
	}

	free = 3 * unit
	controller.tryGrantLocked(0)
	if preparations != 2 {
		t.Fatalf("second pass: root preparations = %d, want 2", preparations)
	}
	if probes != 2 || !third.granted {
		t.Fatalf("second pass: probes = %d, third admitted = %v", probes, third.granted)
	}
}

func TestAdaptiveSchedulerChecksIssueWorkspaceStorage(t *testing.T) {
	t.Parallel()
	const reserve = workspace.MinimumFreeBytes
	for _, tc := range []struct {
		name       string
		storageErr error
		free       uint64
		total      uint64
		force      bool
		wantErr    error
	}{
		{name: "full", total: 2 * reserve},
		{name: "undersized", free: reserve / 2, total: reserve / 2, wantErr: workspace.ErrUnsuitableStorage},
		{name: "force cannot bypass memory mount", storageErr: workspace.ErrUnsuitableStorage, free: 2 * reserve, total: 2 * reserve, force: true, wantErr: workspace.ErrUnsuitableStorage},
		{name: "force cannot bypass free space floor", free: reserve / 2, total: 2 * reserve, force: true, wantErr: workspace.ErrDiskFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			existing := filepath.Join(root, "saved-claim")
			request := TaskDiskRequest{
				Workspace:   workspace.NewManager(),
				ExistingDir: existing,
				CheckPersistentStorage: func(path string) error {
					if path != existing {
						t.Fatalf("checked %q, want %q", path, existing)
					}
					return tc.storageErr
				},
			}
			request.Workspace.Root = root
			free := tc.free
			scheduler := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil, WithSchedulerDiskSpace(func(path string) (string, uint64, uint64, error) {
				if path != existing {
					t.Fatalf("sampled %q, want %q", path, existing)
				}
				return "issue-device", free, tc.total, nil
			}))
			waiter := &taskWaiter{disk: &request, notify: make(chan struct{}, 1)}
			if tc.force {
				_, release, err := scheduler.AcquireWorkspace(context.Background(), tc.name, request, true)
				if release != nil {
					release()
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("force error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			scheduler.waiters = []*taskWaiter{waiter}
			scheduler.tryGrantLocked(0)
			if waiter.granted || !errors.Is(waiter.err, tc.wantErr) {
				t.Fatalf("initial admission granted = %t, error = %v, want %v", waiter.granted, waiter.err, tc.wantErr)
			}
			if tc.wantErr == nil {
				free = reserve
				scheduler.tryGrantLocked(0)
				if !waiter.granted {
					t.Fatal("issue remained queued after workspace space recovered")
				}
				scheduler.releaseFunc(waiter)()
			}
		})
	}
}
