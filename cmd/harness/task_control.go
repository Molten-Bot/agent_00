package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/web"
)

var errTaskStoppedByOperator = errors.New("task was stopped by operator")

type localTaskController struct {
	mu          sync.RWMutex
	tasks       map[string]*localTaskHandle
	persistStop func(string) error
}

type localTaskHandle struct {
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu            sync.Mutex
	paused        bool
	running       bool
	stopped       bool
	finalizing    bool
	forceAcquire  bool
	pauseWait     chan struct{}
	acquireCancel context.CancelFunc
}

func newLocalTaskController() *localTaskController {
	return &localTaskController{
		tasks: map[string]*localTaskHandle{},
	}
}

func (c *localTaskController) Register(requestID string, cancel context.CancelCauseFunc) *localTaskHandle {
	if c == nil {
		return nil
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}

	handle := &localTaskHandle{cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	if c.tasks == nil {
		c.tasks = map[string]*localTaskHandle{}
	}
	c.tasks[requestID] = handle
	c.mu.Unlock()
	return handle
}

func (c *localTaskController) Complete(requestID string) {
	if c == nil {
		return
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}

	c.mu.Lock()
	if handle := c.tasks[requestID]; handle != nil {
		close(handle.done)
	}
	delete(c.tasks, requestID)
	c.mu.Unlock()
}

func (c *localTaskController) waitForCompletion(ctx context.Context, requestID string) error {
	for {
		handle, err := c.handleFor(requestID)
		if err != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-handle.done:
		}
	}
}

func (c *localTaskController) handleFor(requestID string) (*localTaskHandle, error) {
	_, handle, err := c.handleForWithCanonical(requestID)
	return handle, err
}

func (c *localTaskController) handleForWithCanonical(requestID string) (string, *localTaskHandle, error) {
	if c == nil {
		return "", nil, web.ErrTaskNotFound
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", nil, web.ErrTaskNotFound
	}

	c.mu.RLock()
	handle, ok := c.tasks[requestID]
	c.mu.RUnlock()
	if !ok || handle == nil {
		return requestID, nil, web.ErrTaskNotFound
	}
	return requestID, handle, nil
}

func (c *localTaskController) Pause(requestID string) error {
	handle, err := c.handleFor(requestID)
	if err != nil {
		return err
	}
	return handle.Pause()
}

func (c *localTaskController) Run(requestID string) error {
	handle, err := c.handleFor(requestID)
	if err != nil {
		return err
	}
	return handle.Run()
}

func (c *localTaskController) Stop(requestID string) error {
	_, err := c.StopWithCanonical(requestID)
	return err
}

func (c *localTaskController) StopWithCanonical(requestID string) (string, error) {
	canonical, handle, err := c.handleForWithCanonical(requestID)
	if err != nil {
		return canonical, err
	}
	return canonical, handle.stop(func() error {
		if c.persistStop == nil {
			return nil
		}
		return c.persistStop(canonical)
	})
}

func (c *localTaskController) ForceRun(requestID string) error {
	handle, err := c.handleFor(requestID)
	if err != nil {
		return err
	}
	return handle.ForceRun()
}

func (c *localTaskController) Controls(requestID string) web.TaskControls {
	handle, err := c.handleFor(requestID)
	if err != nil {
		return web.TaskControls{}
	}
	return handle.Controls()
}

func (h *localTaskHandle) Pause() error {
	if h == nil {
		return web.ErrTaskNotFound
	}

	var acquireCancel context.CancelFunc
	h.mu.Lock()
	switch {
	case h.stopped:
		h.mu.Unlock()
		return fmt.Errorf("task is already stopped")
	case h.paused:
		h.mu.Unlock()
		return fmt.Errorf("task is already paused")
	default:
		h.paused = true
		if !h.running {
			h.pauseWait = make(chan struct{})
		}
		acquireCancel = h.acquireCancel
		h.mu.Unlock()
	}
	if acquireCancel != nil {
		acquireCancel()
	}
	return nil
}

func (h *localTaskHandle) Run() error {
	if h == nil {
		return web.ErrTaskNotFound
	}

	var pauseWait chan struct{}
	h.mu.Lock()
	switch {
	case h.stopped:
		h.mu.Unlock()
		return fmt.Errorf("task is already stopped")
	case !h.paused:
		h.mu.Unlock()
		return fmt.Errorf("task is not paused")
	default:
		h.paused = false
		pauseWait = h.pauseWait
		h.pauseWait = nil
		h.mu.Unlock()
	}

	if pauseWait != nil {
		close(pauseWait)
	}
	return nil
}

func (h *localTaskHandle) ForceRun() error {
	if h == nil {
		return web.ErrTaskNotFound
	}

	var (
		pauseWait     chan struct{}
		acquireCancel context.CancelFunc
	)
	h.mu.Lock()
	switch {
	case h.stopped:
		h.mu.Unlock()
		return fmt.Errorf("task is already stopped")
	case h.running:
		h.mu.Unlock()
		return fmt.Errorf("task is already running")
	default:
		h.forceAcquire = true
		if h.paused {
			h.paused = false
			pauseWait = h.pauseWait
			h.pauseWait = nil
		}
		acquireCancel = h.acquireCancel
		h.mu.Unlock()
	}

	if pauseWait != nil {
		close(pauseWait)
	}
	if acquireCancel != nil {
		acquireCancel()
	}
	return nil
}

func (h *localTaskHandle) Controls() web.TaskControls {
	if h == nil {
		return web.TaskControls{}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stopped {
		return web.TaskControls{}
	}

	canStop := !h.finalizing
	if h.running {
		if h.paused {
			return web.TaskControls{
				Run:  true,
				Stop: canStop,
			}
		}
		return web.TaskControls{
			Pause: true,
			Stop:  canStop,
		}
	}

	if h.paused {
		return web.TaskControls{
			Run:  true,
			Stop: canStop,
		}
	}

	return web.TaskControls{
		Pause:    true,
		ForceRun: true,
		Stop:     canStop,
	}
}

func (h *localTaskHandle) BeginFinalization() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.finalizing = true
	h.mu.Unlock()
}

func (h *localTaskHandle) stop(persist func() error) error {
	var (
		pauseWait     chan struct{}
		acquireCancel context.CancelFunc
		cancel        context.CancelCauseFunc
	)
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return fmt.Errorf("task is already stopped")
	}
	if h.finalizing {
		h.mu.Unlock()
		return fmt.Errorf("task is finalizing")
	}
	// Persist the stop before canceling, under the same lock as finalization.
	if persist != nil {
		if err := persist(); err != nil {
			h.mu.Unlock()
			return err
		}
	}
	h.stopped = true
	h.paused = false
	h.forceAcquire = false
	pauseWait = h.pauseWait
	h.pauseWait = nil
	acquireCancel = h.acquireCancel
	h.acquireCancel = nil
	cancel = h.cancel
	h.mu.Unlock()

	if pauseWait != nil {
		close(pauseWait)
	}
	if acquireCancel != nil {
		acquireCancel()
	}
	if cancel != nil {
		cancel(errTaskStoppedByOperator)
	}
	return nil
}

func (h *localTaskHandle) SetAcquireCancel(cancel context.CancelFunc) {
	if h == nil {
		if cancel != nil {
			cancel()
		}
		return
	}

	shouldCancel := false
	h.mu.Lock()
	if h.stopped || h.paused {
		shouldCancel = true
	} else {
		h.acquireCancel = cancel
	}
	h.mu.Unlock()
	if shouldCancel && cancel != nil {
		cancel()
	}
}

func (h *localTaskHandle) ClearAcquireCancel(cancel context.CancelFunc) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if cancel != nil {
		h.acquireCancel = nil
	}
	h.mu.Unlock()
}

func (h *localTaskHandle) SetRunning(running bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.running = running
	if running {
		h.forceAcquire = false
	}
	h.mu.Unlock()
}

func (h *localTaskHandle) WaitUntilRunnable(ctx context.Context) error {
	if h == nil {
		return web.ErrTaskNotFound
	}

	for {
		h.mu.Lock()
		if h.stopped {
			h.mu.Unlock()
			return errTaskStoppedByOperator
		}
		if !h.paused {
			h.mu.Unlock()
			return nil
		}
		waitCh := h.pauseWait
		if waitCh == nil {
			waitCh = make(chan struct{})
			h.pauseWait = waitCh
		}
		h.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCh:
		}
	}
}

func (h *localTaskHandle) IsPaused() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

func (h *localTaskHandle) IsStopped() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

func (h *localTaskHandle) ConsumeForceAcquire() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	force := h.forceAcquire
	h.forceAcquire = false
	return force
}

func (h *localTaskHandle) HasForceAcquire() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forceAcquire
}
