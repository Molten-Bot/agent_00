package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func IsInputEvent(event string) bool {
	return event == "app.prompt" || event == "app.json" || event == "app.chat" || event == "app.library"
}

// InputWorkflow selects repository constraints for an app submission.
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
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !IsInputEvent(w.Trigger.Event) || !e.StillEnabled(w) {
		return "", fmt.Errorf("input workflow changed or is disabled")
	}
	doc, err := e.Store.Load()
	if err != nil {
		return "", err
	}
	return e.dispatch(w, doc.DefaultExecution.compose(w), Item{Repository: w.Repository, Kind: "app", Title: "In-app submission"}, "In-app submission", "", submit)
}

// DispatchRetry gives an eligible retry its own operational receipt without
// reading a completed activation. The saved fingerprint binds the retry to the
// original workflow policy, and Tick still enforces current merge gates.
func (e *Engine) DispatchRetry(ctx context.Context, id, policy, priorEvent string, item Item, execution *ExecutionPolicy, submit func(Run) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	doc, err := e.Store.Load()
	if err != nil {
		return "", err
	}
	for _, w := range doc.Workflows {
		if w.ID == id && (w.Merge.Enabled || execution != nil) && w.ExecutionPolicyHash() == policy && CanonicalRepository(w.Repository) == CanonicalRepository(item.Repository) {
			if execution != nil && e.Enabled != nil && e.Allowed != nil && !e.StillEnabled(w) {
				break
			}
			w.Enabled = true
			return e.dispatch(w, execution, item, "Workflow retry", priorEvent, submit)
		}
	}
	return submit(Run{})
}

func (e *Engine) dispatch(w Definition, execution *ExecutionPolicy, item Item, reason, retryOf string, submit func(Run) (string, error)) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	now := e.now()
	run := Run{ID: hex.EncodeToString(random[:]), Workflow: w, Execution: execution, Item: item, Status: "dispatching", Reason: reason, StartedAt: &now, UpdatedAt: now, RetryOf: retryOf}
	// Keep restart reconciliation away from this receipt until submit returns.
	defer func() {
		e.mu.Lock()
		delete(e.submitting, run.ID)
		e.mu.Unlock()
	}()
	err := e.transaction(func(state *State) error {
		if retryOf != "" {
			for _, existing := range state.Runs {
				if existing.RetryOf == retryOf && existing.Status != awaitingReview && !terminal(existing.Status) {
					return fmt.Errorf("workflow retry already active")
				}
			}
			startedAt := state.retryStart(retryOf, now)
			run.StartedAt = &startedAt
		}

		state.Runs = append(state.Runs, run)
		if e.submitting == nil {
			e.submitting = map[string]bool{}
		}
		e.submitting[run.ID] = true
		return nil
	})
	if err != nil {
		return "", err
	}
	e.logRun(run, "dispatch", time.Time{})
	var request string
	var submitErr error
	if IsInputEvent(w.Trigger.Event) && retryOf == "" && !e.StillEnabled(w) {
		submitErr = fmt.Errorf("input workflow changed or is disabled")
	} else {
		request, submitErr = submit(run)
	}
	if err := e.finishSubmission(run.ID, request, submitErr); err != nil {
		return request, err
	}
	return request, submitErr
}

// PRMetadataRetryPolicy permits routing corrections while retaining every other
// captured execution setting. A changed review, task, or merge policy is rejected.
func (e *Engine) PRMetadataRetryPolicy(saved config.WorkflowRunConfig) (config.WorkflowRunConfig, error) {
	doc, err := e.Store.Load()
	if err != nil {
		return saved, err
	}
	for _, w := range doc.Workflows {
		if w.ID != saved.ID {
			continue
		}
		if w.ExecutionPolicyHash() == saved.PolicyHash {
			return saved, nil
		}
		original := w
		original.Task.PRAssignee = saved.PRAssignee
		original.Task.PRLabels = saved.PRLabels
		if original.ExecutionPolicyHash() != saved.PolicyHash {
			return saved, fmt.Errorf("recovery requires the original workflow policy; only PR assignee and labels may change")
		}
		saved.PRAssignee = w.Task.PRAssignee
		saved.PRLabels = append([]string(nil), w.Task.PRLabels...)
		saved.PolicyHash = w.ExecutionPolicyHash()
		return saved, nil
	}
	return saved, fmt.Errorf("originating workflow is unavailable")
}
