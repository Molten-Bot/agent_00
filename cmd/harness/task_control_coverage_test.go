package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/web"
)

func TestLocalTaskControllerNilReceiverGuards(t *testing.T) {
	t.Parallel()

	var controller *localTaskController

	if handle := controller.Register("req", nil); handle != nil {
		t.Fatalf("Register(nil controller) = %v, want nil", handle)
	}
	controller.Complete("req") // should not panic

	if err := controller.Pause("req"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Pause(nil controller) error = %v, want %v", err, web.ErrTaskNotFound)
	}
	if err := controller.Run("req"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Run(nil controller) error = %v, want %v", err, web.ErrTaskNotFound)
	}
	if err := controller.Stop("req"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Stop(nil controller) error = %v, want %v", err, web.ErrTaskNotFound)
	}
	if canonical, err := controller.StopWithCanonical("req"); canonical != "" || !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("StopWithCanonical(nil controller) = (%q, %v), want (\"\", %v)", canonical, err, web.ErrTaskNotFound)
	}
	if err := controller.ForceRun("req"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("ForceRun(nil controller) error = %v, want %v", err, web.ErrTaskNotFound)
	}
	if controls := controller.Controls("req"); controls != (web.TaskControls{}) {
		t.Fatalf("Controls(nil controller) = %#v, want zero value", controls)
	}
}

func TestLocalTaskControllerRegisterOnZeroValueInitializesTasksMap(t *testing.T) {
	t.Parallel()

	var controller localTaskController
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	handle := controller.Register("local-zero", cancel)
	if handle == nil {
		t.Fatal("Register() on zero-value controller returned nil handle")
	}
	if err := controller.Pause("local-zero"); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
}

func TestLocalTaskControllerRegisterEmptyRequestIDReturnsNil(t *testing.T) {
	t.Parallel()

	controller := newLocalTaskController()
	if handle := controller.Register("   ", nil); handle != nil {
		t.Fatalf("Register(empty) = %v, want nil", handle)
	}
}

func TestLocalTaskHandleNilReceiverAccessors(t *testing.T) {
	t.Parallel()

	var handle *localTaskHandle

	if handle.IsPaused() {
		t.Fatal("IsPaused(nil) = true, want false")
	}
	if handle.IsStopped() {
		t.Fatal("IsStopped(nil) = true, want false")
	}
	if handle.ConsumeForceAcquire() {
		t.Fatal("ConsumeForceAcquire(nil) = true, want false")
	}
	if handle.HasForceAcquire() {
		t.Fatal("HasForceAcquire(nil) = true, want false")
	}
	if controls := handle.Controls(); controls != (web.TaskControls{}) {
		t.Fatalf("Controls(nil) = %#v, want zero value", controls)
	}

	handle.SetRunning(true)        // should not panic
	handle.ClearAcquireCancel(nil) // should not panic
}

func TestLocalTaskHandleClearAcquireCancelNilCancelIsNoop(t *testing.T) {
	t.Parallel()

	called := false
	handle := &localTaskHandle{}
	handle.SetAcquireCancel(func() { called = true })

	handle.ClearAcquireCancel(nil)

	handle.mu.Lock()
	stillSet := handle.acquireCancel != nil
	handle.mu.Unlock()
	if !stillSet {
		t.Fatal("ClearAcquireCancel(nil) cleared acquireCancel, want unchanged")
	}
	if called {
		t.Fatal("ClearAcquireCancel(nil) invoked cancel function unexpectedly")
	}
}
