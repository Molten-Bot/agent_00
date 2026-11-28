package workflow

import (
	"crypto/rand"
	"time"

	"github.com/Molten-Bot/agent_00/internal/tasktiming"
)

func (r Run) timingAttempt() string {
	if r.Outcome != nil && r.Outcome.TimingAttempt != "" {
		return r.Outcome.TimingAttempt
	}
	return r.ID
}

func (r Run) waitStage() tasktiming.Stage {
	if terminal(r.Status) {
		return ""
	}
	if r.MergeRequested || (r.AppMerge != nil && r.AppMerge.PendingVerification) {
		return tasktiming.MergeWait
	}
	if r.Status != "waiting" && r.Status != awaitingReview && r.Status != OutcomeChecksPending {
		return ""
	}
	if !r.GitHubRetryAt.IsZero() {
		return tasktiming.GitHub
	}
	if r.Status == OutcomeChecksPending {
		return tasktiming.Checks
	}
	switch r.Reason {
	case "Waiting for successful checks", "Waiting for the trusted review policy check on the current pull request head":
		return tasktiming.Checks
	case "Waiting for pull request to leave draft", "Waiting for requested changes", "Waiting for unresolved review threads",
		"Waiting for human approval", "Waiting for pull request head commit":
		return tasktiming.GitHub
	default:
		return tasktiming.WorkflowWait
	}
}

// Keep the wait's start in the workflow journal across polls and restarts.
func (e *Engine) updateTimingWait(r *Run, waitEnd time.Time) (closed, opened *tasktiming.Span) {
	if e.TimingLog == nil || r.RequestID == "" {
		return nil, nil
	}
	stage := r.waitStage()
	prior := r.TimingWait
	if prior != nil && prior.Stage == stage && prior.Attempt == r.timingAttempt() && waitEnd.IsZero() {
		return nil, nil
	}
	if prior != nil {
		end := r.UpdatedAt
		if !waitEnd.IsZero() {
			end = waitEnd
		}
		span := prior.End(end)
		closed = &span
		r.TimingWait = nil
	}
	if stage != "" {
		span := tasktiming.Start(r.timingAttempt(), "wait-"+rand.Text(), stage, r.UpdatedAt)
		r.TimingWait, opened = &span, &span
	}
	return closed, opened
}

// AcceptTimingAttempt ends a coordinator wait when the same task resumes.
// The new worker records its own queue and work spans.
func (e *Engine) AcceptTimingAttempt(id, requestID, attempt string, acceptedAt time.Time) error {
	if e.TimingLog == nil || id == "" || requestID == "" || attempt == "" || acceptedAt.IsZero() {
		return nil
	}
	return e.transaction(func(s *State) error {
		r := s.run(id)
		if r == nil {
			return errJournalUnchanged
		}
		waiting := r
		if r.RetryOf != "" && r.RequestID == "" {
			waiting = s.run(r.RetryOf)
		}
		if waiting == nil || waiting.Workflow.ID != r.Workflow.ID || waiting.RequestID != requestID ||
			waiting.TimingWait == nil || waiting.TimingWait.Attempt == attempt {
			return errJournalUnchanged
		}
		waiting.closeTimingWait(e.TimingLog, acceptedAt)
		return nil
	})
}

func (r *Run) closeTimingWait(logf tasktiming.Logf, end time.Time) {
	if r.TimingWait != nil {
		// A saved transition can clear or retire the wait. Persist the finish
		// first so replay after a failed journal write can fold it by span ID.
		r.TimingWait.End(end).Write(logf, r.RequestID)
		r.TimingWait = nil
	}
}

func (e *Engine) writeTimingWait(requestID string, closed, opened *tasktiming.Span) {
	if closed != nil {
		closed.Write(e.TimingLog, requestID)
	}
	if opened != nil {
		opened.Write(e.TimingLog, requestID)
	}
}
