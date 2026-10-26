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
	controller.RegisterAliases("req", "alias") // should not panic
	controller.Complete("req")                 // should not panic

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

func TestLocalTaskControllerRegisterAliasesEdgeCases(t *testing.T) {
	t.Parallel()

	controller := newLocalTaskController()

	// Empty request ID is a no-op.
	controller.RegisterAliases("   ", "alias")

	// Unregistered canonical request ID is a no-op.
	controller.RegisterAliases("missing-task", "alias-for-missing")
	if err := controller.Pause("alias-for-missing"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Pause(unregistered alias) error = %v, want %v", err, web.ErrTaskNotFound)
	}

	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	controller.Register("req-aliases", cancel)

	// Aliases equal to empty string or to the canonical ID itself are skipped.
	controller.RegisterAliases("req-aliases", "", "req-aliases", "real-alias")

	if err := controller.Pause("real-alias"); err != nil {
		t.Fatalf("Pause(real-alias) error = %v", err)
	}
	if err := controller.Run("real-alias"); err != nil {
		t.Fatalf("Run(real-alias) error = %v", err)
	}
}

func TestLocalTaskControllerRegisterAliasesOnControllerWithNilAliasesMap(t *testing.T) {
	t.Parallel()

	controller := &localTaskController{tasks: map[string]*localTaskHandle{}}
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	controller.Register("req-nil-aliases", cancel)
	controller.aliases = nil

	controller.RegisterAliases("req-nil-aliases", "alias-nil-map")
	if err := controller.Pause("alias-nil-map"); err != nil {
		t.Fatalf("Pause(alias-nil-map) error = %v", err)
	}
}

func TestLocalTaskControllerCompleteClearsAliasesPointingAtRequest(t *testing.T) {
	t.Parallel()

	controller := newLocalTaskController()
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	controller.Register("req-complete", cancel)
	controller.RegisterAliases("req-complete", "alias-a", "alias-b")

	controller.Complete("alias-a")

	if err := controller.Pause("req-complete"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Pause(completed canonical via alias) error = %v, want %v", err, web.ErrTaskNotFound)
	}
	if err := controller.Pause("alias-b"); !errors.Is(err, web.ErrTaskNotFound) {
		t.Fatalf("Pause(remaining alias after completion) error = %v, want %v", err, web.ErrTaskNotFound)
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
