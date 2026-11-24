package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func IsInputEvent(event string) bool {
	return event == "app.prompt" || event == "app.json" || event == "app.chat"
}

// InputWorkflow selects an explicit replacement for configuration defaults.
// Ambiguous overrides are rejected rather than dispatching duplicate tasks.
func (e *Engine) InputWorkflow(repository, source string) (*Definition, error) {
	if !IsInputEvent("app."+source) || !e.Enabled() {
		return nil, nil
	}
	doc, err := e.Store.Load()
	if err != nil {
		return nil, err
	}
	var selected *Definition
	for _, w := range doc.Workflows {
		if w.Enabled && w.Trigger.Event == "app."+source && CanonicalRepository(w.Repository) == CanonicalRepository(repository) {
			if selected != nil {
				return nil, fmt.Errorf("multiple enabled %s workflows match this repository; pause all but one", source)
			}
			copy := w
			selected = &copy
		}
	}
	return selected, nil
}

// DispatchInput journals submission before enqueueing, using the same completion
// and merge gates as scheduled and GitHub-triggered work.
func (e *Engine) DispatchInput(ctx context.Context, w Definition, submit func(Run) (string, error)) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !IsInputEvent(w.Trigger.Event) || !e.StillEnabled(w) {
		return "", fmt.Errorf("input workflow changed or is disabled")
	}
	return e.dispatch(w, Item{Repository: w.Repository, Kind: "app", Title: "In-app submission"}, "In-app submission", "", submit)
}

// DispatchRetry gives an eligible retry its own operational receipt without
// reading a completed activation. The saved fingerprint binds the retry to the
// original workflow policy, and Tick still enforces current merge gates.
func (e *Engine) DispatchRetry(ctx context.Context, id, policy, priorEvent string, item Item, submit func(Run) (string, error)) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	doc, err := e.Store.Load()
	if err != nil {
		return "", err
	}
	for _, w := range doc.Workflows {
		if w.ID == id && w.Merge.Enabled && w.ExecutionPolicyHash() == policy && CanonicalRepository(w.Repository) == CanonicalRepository(item.Repository) {
			w.Enabled = true
			return e.dispatch(w, item, "Workflow retry", priorEvent, submit)
		}
	}
	return submit(Run{})
}

// dispatch must run with the engine mutex held.
func (e *Engine) dispatch(w Definition, item Item, reason, retryOf string, submit func(Run) (string, error)) (string, error) {
	state, err := e.load()
	if err != nil {
		return "", err
	}
	if retryOf != "" {
		for _, existing := range state.Runs {
			if existing.RetryOf == retryOf {
				return "", fmt.Errorf("workflow retry already active")
			}
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	run := Run{ID: hex.EncodeToString(random[:]), Workflow: w, Item: item, Status: "dispatching", Reason: reason, UpdatedAt: e.now(), RetryOf: retryOf}
	state.Runs = append(state.Runs, run)
	if err := e.save(state); err != nil {
		return "", err
	}
	e.logRun(run, "dispatch")
	request, submitErr := submit(run)
	run.RequestID = request
	run.UpdatedAt = e.now()
	run.Status = "running"
	if submitErr != nil {
		run.Status = "failed"
		run.Reason = "Submission failed: " + submitErr.Error()
	}
	state.Runs[len(state.Runs)-1] = run
	if err := e.save(state); err != nil {
		return request, err
	}
	e.logRun(run, "submission")
	return request, submitErr
}
