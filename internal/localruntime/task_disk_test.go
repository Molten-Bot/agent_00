package localruntime

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestTaskDiskHistoryPersistsOnlyLargerMeasurements(t *testing.T) {
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
	stamp := time.Unix(1000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := history.Record(key, 42); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatal("smaller measurement rewrote the history file")
	}

	reopened, err := OpenTaskDiskHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RequiredBytes(alias); got != 256 {
		t.Fatalf("reopened disk requirement = %d, want 256", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "example") {
		t.Fatal("history contains a raw repository reference")
	}
}

func TestAdaptiveSchedulerReservesSpacePerFilesystem(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	controller.sample = staticSample{}
	root, sameVolume, other := t.TempDir(), t.TempDir(), t.TempDir()
	const unit = workspace.MinimumFreeBytes
	controller.diskSpace = func(path string) (string, uint64, error) {
		if path == other {
			return "other-volume", 3 * unit, nil
		}
		return "shared-volume", 3 * unit, nil
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
	controller.diskSpace = func(path string) (string, uint64, error) {
		if filepath.Dir(path) == ram {
			return ram, workspace.MinimumFreeBytes, nil
		}
		return disk, 3 * workspace.MinimumFreeBytes, nil
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
	controller.diskSpace = func(string) (string, uint64, error) {
		return "disk", free, nil
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

func TestAdaptiveSchedulerReportsWorkspaceSetupFailure(t *testing.T) {
	t.Parallel()

	for _, force := range []bool{false, true} {
		controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
		controller.diskSpace = func(string) (string, uint64, error) {
			return "disk", workspace.MinimumFreeBytes, nil
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

func TestAdaptiveSchedulerTriesAnotherWorkspaceAfterSetupFailure(t *testing.T) {
	ram, disk := t.TempDir(), t.TempDir()
	t.Setenv("HARNESS_WORKSPACE_RAM_BASE", ram)
	t.Setenv("HARNESS_WORKSPACE_DISK_BASE", disk)
	t.Setenv("HARNESS_WORKSPACE_ROOT_NAME", "test-workspaces")

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 1}, nil)
	controller.diskSpace = func(path string) (string, uint64, error) {
		return filepath.Dir(path), workspace.MinimumFreeBytes, nil
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

func TestAdaptiveSchedulerReservesFullForcedEstimate(t *testing.T) {
	t.Parallel()

	controller := NewAdaptiveScheduler(SchedulerConfig{MaxParallel: 3}, nil)
	free := uint64(workspace.MinimumFreeBytes)
	controller.diskSpace = func(string) (string, uint64, error) {
		return "disk", free, nil
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
	controller.diskSpace = func(string) (string, uint64, error) {
		probes++
		return "disk", 0, nil
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
