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
	state, err := e.load()
	if err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	run := Run{ID: hex.EncodeToString(random[:]), Workflow: w, Item: Item{Repository: w.Repository, Kind: "app", Title: "In-app submission"}, Status: "dispatching", Reason: "In-app submission", UpdatedAt: e.now()}
	state.Runs = append(state.Runs, run)
	if err := writeAtomic(e.StatePath, state); err != nil {
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
	if err := writeAtomic(e.StatePath, state); err != nil {
		return request, err
	}
	e.logRun(run, "submission")
	return request, submitErr
}
