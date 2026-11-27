package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/githubutil"
)

type Item struct {
	HumanApproved  bool     `json:"humanApproved"`
	Merged         bool     `json:"merged,omitempty"`
	Viewer         string   `json:"viewer,omitempty"`
	Number         int      `json:"number"`
	Repository     string   `json:"repository"`
	Kind           string   `json:"kind"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	URL            string   `json:"url"`
	Author         string   `json:"author"`
	Bot            bool     `json:"bot"`
	Labels         []string `json:"labels"`
	Assignees      []string `json:"assignees"`
	Reviewers      []string `json:"reviewers"`
	IssueType      string   `json:"issueType"`
	IssueStatus    string   `json:"issueStatus,omitempty"`
	HasIssueStatus bool     `json:"hasIssueStatus,omitempty"`
	BaseBranch     string   `json:"baseBranch"`
	HeadBranch     string   `json:"headBranch"`
	HeadSHA        string   `json:"headSha"`
	Draft          bool     `json:"draft"`
	LinkedIssue    bool     `json:"linkedIssue"`
	ReviewDecision string   `json:"reviewDecision"`
	ChecksFailed   bool     `json:"checksFailed"`
	ChecksPassed   bool     `json:"checksPassed"`
	Open           bool     `json:"open"`

	MergedAt time.Time `json:"mergedAt,omitzero"`

	ActionRunID      int64 `json:"actionRunId,omitempty"`
	ActionRunAttempt int   `json:"actionRunAttempt,omitempty"`
}
type Decision struct {
	Item    Item   `json:"item"`
	Matches bool   `json:"matches"`
	Reason  string `json:"reason"`
}

func Match(w Definition, i Item) (bool, string) {
	if !i.Open && w.Trigger.Event != ActionFailedEvent {
		return false, "Item is closed"
	}
	if !strings.EqualFold(w.Repository, i.Repository) {
		return false, "Repository does not match"
	}
	kind := strings.Split(w.Trigger.Event, ".")[0]
	if i.Kind != kind {
		return false, "Item type does not match"
	}
	if i.Kind == "issue" && i.HasIssueStatus && !strings.EqualFold(i.IssueStatus, "Todo") {
		return false, "Issue planning status must be Todo"
	}
	if w.Trigger.Event == ActionFailedEvent {
		if !i.ChecksFailed {
			return false, "Run did not complete with failure"
		}
		if strings.HasPrefix(i.HeadBranch, "agent00-") {
			return false, "Run is on an agent work branch"
		}
		return true, "GitHub Actions run failed"
	}
	triggerValue := w.Trigger.Value
	if triggerValue == "@me" {
		triggerValue = i.Viewer
	}
	matched := false
	switch w.Trigger.Event {
	case "issue.opened", "pr.opened":
		matched = true
	case "issue.assigned", "pr.assigned":
		matched = has(i.Assignees, triggerValue)
	case "issue.labeled", "pr.labeled":
		matched = has(i.Labels, w.Trigger.Value)
	case "pr.ready":
		matched = !i.Draft
	case "pr.review_requested":
		matched = has(i.Reviewers, triggerValue)
	case "pr.changes_requested":
		matched = i.ReviewDecision == "CHANGES_REQUESTED"
	case "pr.checks_failed":
		matched = i.ChecksFailed
	}
	if !matched {
		return false, "Trigger does not match"
	}
	matches := 0
	for _, r := range w.Filters.Rules {
		var values []string
		switch r.Field {
		case "label":
			values = i.Labels
		case "author":
			values = []string{i.Author}
		case "assignee":
			values = i.Assignees
		case "reviewer":
			values = i.Reviewers
		case "issueType":
			values = []string{i.IssueType}
		case "baseBranch":
			values = []string{i.BaseBranch}
		case "draft":
			values = []string{fmt.Sprint(i.Draft)}
		case "bot":
			values = []string{fmt.Sprint(i.Bot)}
		case "linkedIssue":
			values = []string{fmt.Sprint(i.LinkedIssue)}
		}
		value := r.Value
		if value == "@me" && contains([]string{"author", "assignee", "reviewer"}, r.Field) {
			value = i.Viewer
		}
		ok := value != "" && has(values, value)
		if r.Operator == "isNot" {
			ok = !ok
		}
		if ok {
			matches++
		}
	}
	if len(w.Filters.Rules) > 0 && ((w.Filters.Mode == "all" && matches != len(w.Filters.Rules)) || (w.Filters.Mode == "any" && matches == 0)) {
		return false, fmt.Sprintf("Matched %d of %d filters (%s)", matches, len(w.Filters.Rules), w.Filters.Mode)
	}
	return true, "Trigger and filters match"
}
func has(values []string, value string) bool {
	for _, v := range values {
		if strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (w Definition) ExecutionPolicyHash() string {
	w.Positions = nil
	w.Name = ""
	w.Enabled = false
	return digest(w)
}

type Run struct {
	Execution     *ExecutionPolicy `json:"execution,omitempty"`
	Outcome       *Outcome         `json:"outcome,omitempty"`
	ID            string           `json:"id"`
	RetryOf       string           `json:"retryOf,omitempty"`
	Workflow      Definition       `json:"workflow"`
	Item          Item             `json:"item"`
	RequestID     string           `json:"requestId,omitempty"`
	Status        string           `json:"status"`
	Reason        string           `json:"reason"`
	PRURL         string           `json:"prUrl,omitempty"`
	MergeHead     string           `json:"mergeHead,omitempty"`
	AppMerge      *appMergeReceipt `json:"appMerge,omitempty"`
	StartedAt     *time.Time       `json:"startedAt,omitempty"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	GitHubRetryAt time.Time        `json:"githubRetryAt,omitzero"`
}

// Retain a successful app merge until its issue claim receipt is written.
type appMergeReceipt struct {
	Title    string    `json:"title,omitempty"`
	MergedAt time.Time `json:"mergedAt,omitzero"`
}

func (r Run) LinkedPRURL() string {
	if r.PRURL != "" {
		return r.PRURL
	}
	if r.Outcome != nil && r.Outcome.PRURL != "" {
		return r.Outcome.PRURL
	}
	return ""
}

func (r Run) ReviewPolicy() ExecutionPolicy {
	if r.Execution != nil {
		return *r.Execution
	}
	return ExecutionPolicy{ReviewCycles: r.Workflow.ReviewCycles, ReviewAgent: r.Workflow.ReviewAgent}
}

type observation struct {
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
}
type State struct {
	Schedules    map[string]ScheduleCursor `json:"schedules,omitempty"`
	Version      int                       `json:"version"`
	Observations map[string]observation    `json:"observations"`
	Baselines    map[string]bool           `json:"baselines"`
	Runs         []Run                     `json:"runs"`
}

// OutcomeReviewPending retains an activation while allowing fresh review work.
const OutcomeReviewPending = "review_pending"

// OutcomeChecksPending reserves the existing activation until its worker resumes.
const OutcomeChecksPending = "check_verification_pending"

type Outcome struct {
	ReviewReceipt     *githubutil.ReviewReceipt `json:"reviewReceipt,omitempty"`
	ReviewInterrupted bool                      `json:"reviewInterrupted,omitempty"`
	Status            string                    `json:"status"`
	PRURL             string                    `json:"prUrl,omitempty"`
	Reason            string                    `json:"reason,omitempty"`

	PendingReviewGate *githubutil.PendingReviewGate `json:"pendingReviewGate,omitempty"`
	ReviewRequired    bool                          `json:"reviewRequired,omitempty"`
	// Accept older journals without using their unacknowledged review verdicts.
	LegacyReviewClean  bool   `json:"reviewClean,omitempty"`
	LegacyReviewedHead string `json:"reviewedHead,omitempty"`
}
type Engine struct {
	Now       func() time.Time
	Active    func(Definition) bool
	Store     *Store
	StatePath string
	Enabled   func() bool
	Allowed   func(string) bool
	Fetch     func(context.Context, string, ScanNeeds) ([]Item, error)
	Submit    func(context.Context, Run) (string, error)
	Status    func(string) Outcome
	Reconcile func(string) string
	InspectPR func(context.Context, string, string) (Item, error)
	Merge     func(context.Context, Run) error
	Logf      func(string, ...any)

	FinishReview func(context.Context, githubutil.ReviewReceipt, *githubutil.PendingReviewGate) error
	// RecordIssueMerge saves the merge before pruning an issue activation.
	RecordIssueMerge func(previous Run, pr Item, mergedByApp bool) error
	// MemoryPressure defers background ticks while host memory is exhausted.
	MemoryPressure func() bool

	wakeOnce   sync.Once
	wake       chan struct{}
	tickMu     sync.Mutex
	submitting map[string]bool // Live input reservations, guarded by mu.
	mu         sync.Mutex
}

var ErrIssueAdmissionPending = errors.New("waiting for issue claim recovery")

var ErrActivationNotFound = errors.New("workflow activation not found")

const awaitingReview = "awaiting_review"
const observedIssueMergeReason = "Issue pull request merge observed"

func (e *Engine) load() (State, error) {
	s := State{Version: 2, Observations: map[string]observation{}, Baselines: map[string]bool{}, Runs: []Run{}}
	b, err := os.ReadFile(e.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = decodeStrict(b, &s); err != nil {
		return s, err
	}
	if s.Version == 1 {
		s.Version = 2
	}
	if s.Version != 2 || s.Observations == nil || s.Baselines == nil {
		return s, fmt.Errorf("unsupported workflow runtime state")
	}
	return s, nil
}
func (e *Engine) Runs() ([]Run, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.load()
	return s.Runs, err
}

// save persists only runs that still need dispatch recovery or merge coordination.
// Keep the caller's slice intact so it can log a terminal transition after saving.
func (e *Engine) save(s State) error {
	active := make([]Run, 0, len(s.Runs))
	for _, run := range s.Runs {
		if !terminal(run.Status) || run.hasFailedPendingReview() {
			active = append(active, run)
		}
	}
	s.Runs = active
	return writeAtomic(e.StatePath, s)
}

// logRun emits only bounded lifecycle reasons. Detailed errors can contain
// repository URLs, paths, or task instructions and remain in the run journal.
func (e *Engine) logRun(r Run, event string) {
	if e.Logf == nil {
		return
	}
	reason := r.Status
	switch r.Reason {
	case "Waiting for checks and merge requirements":
		reason = "merge_requirements"
	case "Waiting for pull request to leave draft":
		reason = "draft_pull_request"
	case "Waiting for requested changes":
		reason = "changes_requested"
	case "Waiting for unresolved review threads":
		reason = "review_threads_pending"
	case "Waiting for successful checks":
		reason = "checks_pending"
	case "Waiting for human approval":
		reason = "approval_pending"
	case "Waiting for pull request head commit":
		reason = "head_missing"
	case "Head changed after merge eligibility; a new review is required":
		reason = "head_changed"
	case "Merge paused: workflow feature, workflow, or repository is disabled":
		reason = "merge_paused"
	case "Workflow changed or was disabled before merge":
		reason = "workflow_changed"
	case "Waiting for a submitted clean review on the current pull request head":
		reason = "review_not_clean"
	case "Waiting for the trusted review policy check on the current pull request head":
		reason = "review_check_pending"
	case "Review writeback paused: workflow changed or was disabled":
		reason = "review_writeback_paused"
	case "Pull request merged externally during review":
		reason = "external_merge"
	case observedIssueMergeReason:
		reason = "external_merge"
	case "Pull request was already merged when the workflow checked":
		reason = "merge_unconfirmed"
	}
	if strings.HasPrefix(r.Reason, "Merge blocked:") {
		reason = "merge_blocked"
	}
	if strings.HasPrefix(r.Reason, "Review writeback pending:") {
		reason = "review_writeback_pending"
	}
	if !r.GitHubRetryAt.IsZero() {
		reason = "github_rate_limit"
	}
	durationMS := "unknown"
	if terminal(r.Status) && r.StartedAt != nil && !r.StartedAt.IsZero() &&
		!r.UpdatedAt.IsZero() && !r.UpdatedAt.Before(*r.StartedAt) {
		durationMS = strconv.FormatInt(r.UpdatedAt.Sub(*r.StartedAt).Milliseconds(), 10)
	}
	e.Logf("workflow event=%s workflow_id=%q run_id=%q request_id=%q status=%s reason=%s duration_ms=%s", event, r.Workflow.ID, r.ID, r.RequestID, r.Status, reason, durationMS)
}
func (e *Engine) Preview(ctx context.Context, w Definition) ([]Decision, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	if !e.Allowed(w.Repository) {
		return nil, fmt.Errorf("repository is disabled in Settings")
	}
	if IsInputEvent(w.Trigger.Event) {
		return []Decision{{Item: Item{Repository: w.Repository, Kind: "app", Title: "In-app submission"}, Matches: true, Reason: "Runs when this repository receives a " + strings.TrimPrefix(w.Trigger.Event, "app.") + " submission; without an enabled override, configuration defaults apply"}}, nil
	}
	if w.Trigger.Event == "schedule" {
		return []Decision{{Item: Item{Repository: w.Repository, Kind: "schedule", Title: "Scheduled repository task"}, Matches: true, Reason: fmt.Sprintf("Runs every %g hours while enabled; no GitHub event is required", w.Trigger.IntervalHours)}}, nil
	}
	var needs ScanNeeds
	needs.include(w)
	items, err := e.Fetch(githubutil.WithCaller(ctx, "workflow_preview"), w.Repository, needs)
	kind := strings.Split(w.Trigger.Event, ".")[0]
	if !usableSnapshot(kind, err) {
		return nil, err
	}
	out := []Decision{}
	for _, i := range items {
		if i.Kind != kind {
			continue
		}
		ok, reason := Match(w, i)
		out = append(out, Decision{i, ok, reason})
	}
	return out, nil
}

func usableSnapshot(kind string, err error) bool {
	var partial *pullRequestSnapshotError
	return err == nil || (kind == "issue" && errors.As(err, &partial))
}

func (e *Engine) wakeChannel() chan struct{} {
	e.wakeOnce.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

// RequestScan queues a normal polling cycle without interrupting one in progress.
func (e *Engine) RequestScan() {
	select {
	case e.wakeChannel() <- struct{}{}:
	default:
	}
}

func (e *Engine) Run(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	wake := e.wakeChannel()
	for ctx.Err() == nil {
		if err := e.Tick(ctx); err != nil && e.Logf != nil {
			var rate *githubutil.RateLimitError
			if errors.As(err, &rate) {
				e.Logf("workflow status=waiting reason=github_rate_limit retry_at=%s", rate.RetryAt.UTC().Format(time.RFC3339))
			} else {
				e.Logf("workflow status=error err=%q", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		}
	}
}
func (e *Engine) Tick(ctx context.Context) error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if e.MemoryPressure != nil && e.MemoryPressure() {
		return nil
	}
	featureEnabled := e.Enabled()
	d, err := e.Store.Load()
	if err != nil {
		return err
	}
	e.mu.Lock()
	s, err := e.load()
	var runs []Run
	for _, r := range s.Runs {
		if !e.submitting[r.ID] && (!terminal(r.Status) || r.hasFailedPendingReview()) {
			runs = append(runs, r)
		}
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	definitions := map[string]Definition{}
	for _, w := range d.Workflows {
		definitions[w.ID] = w
	}
	var errs []error
	for _, r := range runs {
		if e.MemoryPressure != nil && e.MemoryPressure() {
			return errors.Join(errs...)
		}
		w, exists := definitions[r.Workflow.ID]
		if err := e.reconcileRun(ctx, r, w, exists, featureEnabled); err != nil {
			errs = append(errs, err)
		}
	}
	type snapshot struct {
		items []Item
		err   error
	}
	type scanKey struct{ repository, actionWorkflow string }
	needsByRepo := map[string]ScanNeeds{}
	for _, w := range d.Workflows {
		if w.Enabled && (strings.HasPrefix(w.Trigger.Event, "issue.") || strings.HasPrefix(w.Trigger.Event, "pr.")) {
			key := strings.ToLower(w.Repository)
			needs := needsByRepo[key]
			needs.include(w)
			needsByRepo[key] = needs
		}
	}
	cache := map[scanKey]snapshot{}
	reportedErrors := map[scanKey]bool{}
	for _, w := range d.Workflows {
		if e.MemoryPressure != nil && e.MemoryPressure() {
			break
		}
		if !featureEnabled || !w.Enabled || !e.Allowed(w.Repository) {
			continue
		}
		if IsInputEvent(w.Trigger.Event) {
			continue
		}
		if w.Trigger.Event == "schedule" {
			if err := e.tickSchedule(ctx, w, d.DefaultExecution.compose(w), s); err != nil {
				return err
			}
			continue
		}
		key := strings.ToLower(w.Repository)
		needs := needsByRepo[key]
		action := w.Trigger.Event == ActionFailedEvent
		if action {
			needs = ScanNeeds{}
			needs.include(w)
		}
		source := scanKey{repository: key, actionWorkflow: needs.ActionWorkflow}
		fetched, ok := cache[source]
		if !ok {
			fetched.items, fetched.err = e.Fetch(ctx, w.Repository, needs)
			cache[source] = fetched
		}
		kind := strings.Split(w.Trigger.Event, ".")[0]
		if !usableSnapshot(kind, fetched.err) {
			if !reportedErrors[source] {
				errs = append(errs, fetched.err)
				reportedErrors[source] = true
			}
			continue
		}
		if !e.StillEnabled(w) {
			continue
		}
		// Configuration edits establish a fresh baseline; layout/name edits do not.
		baselineKey := w.ID + ":" + w.ExecutionPolicyHash()
		hadBaseline := s.Baselines[baselineKey]
		seen := make(map[string]bool, len(fetched.items))
		for _, i := range fetched.items {
			if i.Kind != kind {
				continue
			}
			entity := fmt.Sprintf("%s#%d", key, i.Number)
			if action {
				entity = fmt.Sprintf("%s/actions/runs/%d", key, i.ActionRunID)
			}
			obsKey := baselineKey + ":" + entity
			seen[obsKey] = true
			old := s.Observations[obsKey]
			match, reason := Match(w, i)
			fingerprint := ""
			if match {
				fingerprint = w.Trigger.Event
			}
			if action {
				// Run IDs survive reruns. Retain the highest observed failed
				// attempt across queued/successful states and polling-window changes.
				priorAttempt, _ := strconv.Atoi(old.Fingerprint)
				if !match || i.ActionRunAttempt <= priorAttempt {
					continue
				}
				fingerprint = strconv.Itoa(i.ActionRunAttempt)
			}
			if old.Fingerprint == fingerprint {
				continue
			}
			// Keep observing a match becoming false while work is active. Only
			// defer admission of a new match, otherwise a later false-to-true
			// transition is lost permanently when the current task finishes.
			old.Fingerprint = fingerprint
			old.Generation++
			if !match || (!hadBaseline && !w.IncludeBacklog) {
				s.Observations[obsKey] = old
				continue
			}
			now := e.now()
			r := Run{ID: digest([]any{obsKey, fingerprint, old.Generation}), Workflow: w, Execution: d.DefaultExecution.compose(w), Item: i, Status: "dispatching", Reason: reason, StartedAt: &now, UpdatedAt: now}
			reserved := false
			err := e.transaction(func(current *State) error {
				if current.busy(i) {
					// Persist the highest observed attempt even when another repair
					// for this run is active. A later successful rerun must not hide it.
					if action {
						current.Observations[obsKey] = old
						r.Status = "pending_action"
						current.Runs = append(current.Runs, r)
						return nil
					}
					return errJournalUnchanged
				}
				current.Observations[obsKey] = old
				current.Runs = append(current.Runs, r)
				reserved = true
				return nil
			})
			if err != nil {
				return err
			}
			if !reserved {
				if action {
					s.Observations[obsKey] = old
				}
				continue
			}
			s.Observations[obsKey] = old
			if err := e.submitReserved(ctx, r); err != nil {
				if !errors.Is(err, ErrIssueAdmissionPending) {
					return err
				}
				errs = append(errs, err)
			}
		}
		if !action {
			// The issue/PR snapshot contains every open item. Record closed
			// items as non-matches so reopening can activate again.
			for obsKey, old := range s.Observations {
				if strings.HasPrefix(obsKey, baselineKey+":") && !seen[obsKey] && old.Fingerprint != "" {
					old.Fingerprint = ""
					old.Generation++
					s.Observations[obsKey] = old
				}
			}
		}
		s.Baselines[baselineKey] = true
	}
	// Only the tick coordinator writes observations and baselines.
	if err := e.transaction(func(current *State) error {
		current.Observations, current.Baselines = s.Observations, s.Baselines
		return nil
	}); err != nil {
		return err
	}
	return errors.Join(errs...)
}

func (s *State) run(id string) *Run {
	for n := range s.Runs {
		if s.Runs[n].ID == id {
			return &s.Runs[n]
		}
	}
	return nil
}

func (s *State) busy(item Item) bool {
	for _, run := range s.Runs {
		if run.Status != "pending_action" && run.Status != awaitingReview && !terminal(run.Status) && strings.EqualFold(run.Item.Repository, item.Repository) &&
			run.Item.Number == item.Number && run.Item.ActionRunID == item.ActionRunID {
			return true
		}
	}
	return false
}

var errJournalUnchanged = errors.New("no workflow journal update")

// transaction reloads the last committed journal. Callbacks only modify local state.
func (e *Engine) transaction(change func(*State) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.load()
	if err != nil {
		return err
	}
	if err := change(&s); err != nil {
		if err == errJournalUnchanged {
			return nil
		}
		return err
	}
	return e.save(s)
}

func (e *Engine) submitReserved(ctx context.Context, r Run) error {
	e.logRun(r, "dispatch")
	var request string
	var submitErr error
	if !e.StillEnabled(r.Workflow) {
		submitErr = errors.New("workflow changed or was disabled before submission")
	} else {
		request, submitErr = e.Submit(ctx, r)
	}
	if err := e.finishSubmission(r.ID, request, submitErr); err != nil {
		return err
	}
	if errors.Is(submitErr, ErrIssueAdmissionPending) {
		return submitErr
	}
	return nil
}

func (e *Engine) finishSubmission(id, request string, submitErr error) error {
	var result Run
	err := e.transaction(func(s *State) error {
		r := s.run(id)
		if r == nil {
			return ErrActivationNotFound
		}
		r.RequestID = request
		r.UpdatedAt = e.now()
		r.Status = "running"
		if errors.Is(submitErr, ErrIssueAdmissionPending) {
			r.Status = "pending_issue"
			r.Reason = submitErr.Error()
		} else if submitErr != nil && r.Outcome == nil {
			// A worker may already have recorded completion before submit returns.
			r.Status = "failed"
			r.Reason = "Submission failed: " + submitErr.Error()
		}
		if submitErr == nil && request != "" && r.RetryOf != "" {
			if prior := s.run(r.RetryOf); prior != nil && prior.RequestID == request && prior.Workflow.ID == r.Workflow.ID {
				prior.Status, prior.Reason = "completed", "Superseded by same-task retry"
			}
		}
		result = *r
		return nil
	})
	if err == nil {
		e.logRun(result, "submission")
	}
	return err
}

func sameOutcome(a, b *Outcome) bool { return reflect.DeepEqual(a, b) }

func (e *Engine) reconcileRun(ctx context.Context, original Run, w Definition, exists, featureEnabled bool) error {
	r := original
	// Only Complete can change a run being reconciled. Live submissions are
	// excluded from the tick snapshot. Compare the outcome before replacing it.
	expected := original
	stale := false
	var mergedPR Item
	mergedByApp := false
	commit := func() error {
		return e.transaction(func(s *State) error {
			current := s.run(r.ID)
			if current == nil || !sameOutcome(current.Outcome, expected.Outcome) || current.Status != expected.Status || current.RequestID != expected.RequestID {
				stale = true
				return errJournalUnchanged
			}
			if r.Status == "merged" && r.Item.Kind == "issue" {
				previous := *current
				if r.Outcome != nil && r.Outcome.ReviewInterrupted {
					previous.Outcome = r.Outcome
				}
				if err := e.recordIssueMerge(previous, r.PRURL, mergedPR, mergedByApp); err != nil {
					return err
				}
			}
			*current = r
			expected = r
			return nil
		})
	}
	finish := func() error {
		r.UpdatedAt = e.now()
		if r.Status == "merged" {
			r.GitHubRetryAt = time.Time{}
		}
		if err := commit(); err != nil {
			return err
		}
		if !stale && (original.Status != r.Status || original.Reason != r.Reason) {
			e.logRun(r, "transition")
		}
		return nil
	}
	waitForQuota := func(err error) bool {
		var rate *githubutil.RateLimitError
		if !errors.As(err, &rate) {
			return false
		}
		r.GitHubRetryAt = rate.RetryAt
		r.Reason = "Waiting for GitHub quota. " + rate.Error()
		return true
	}
	if r.AppMerge != nil {
		mergedPR = Item{Title: r.AppMerge.Title, MergedAt: r.AppMerge.MergedAt}
		mergedByApp = true
		r.Status, r.Reason = "merged", "Pull request merged"
		return finish()
	}
	if r.Item.Kind == "issue" && r.Outcome != nil && r.Outcome.ReviewInterrupted && r.LinkedPRURL() != "" {
		r.PRURL = r.LinkedPRURL()
		r.Status, r.Reason = "merged", observedIssueMergeReason
		return finish()
	}
	active := featureEnabled && exists && w.Enabled && e.Allowed(w.Repository) && e.Allowed(r.Item.Repository)
	if !e.now().Before(r.GitHubRetryAt) {
		r.GitHubRetryAt = time.Time{}
	} else if active {
		return nil
	}
	if r.Status == "pending_action" {
		if !active || !e.StillEnabled(r.Workflow) {
			r.Status, r.Reason = "completed", "Workflow changed or was disabled before queued repair"
			return finish()
		}
		reserved := false
		if err := e.transaction(func(s *State) error {
			current := s.run(r.ID)
			if current == nil || current.Status != "pending_action" || s.busy(r.Item) {
				return errJournalUnchanged
			}
			current.Status = "dispatching"
			current.UpdatedAt = e.now()
			r = *current
			reserved = true
			return nil
		}); err != nil {
			return err
		}
		if reserved {
			return e.submitReserved(ctx, r)
		}
		return nil
	}
	if r.Status == "dispatching" || r.Status == "pending_issue" {
		if r.RequestID == "" && e.Reconcile != nil {
			r.RequestID = e.Reconcile(r.ID)
		}
		if r.RequestID == "" && r.Outcome == nil {
			if r.Status == "dispatching" {
				r.Reason = "Submission outcome unknown after restart; retained to prevent duplicate work"
			}
			return finish()
		}
		if r.Status == "pending_issue" {
			r.Reason = "Issue claim recovered"
		}
		r.Status = "running"
	}
	if r.Status == OutcomeChecksPending {
		return nil
	}
	if r.Status == "running" || r.hasFailedPendingReview() {
		var o Outcome
		if r.Outcome != nil {
			o = *r.Outcome
		} else {
			o = e.Status(r.RequestID)
		}
		if o.Status == "failed" || o.Status == "succeeded" || o.Status == OutcomeReviewPending || o.Status == OutcomeChecksPending {
			r.Outcome = &o
		}
		if o.ReviewInterrupted {
			r.PRURL = o.PRURL
			if r.Item.Kind == "issue" {
				if r.PRURL == "" {
					r.PRURL = original.PRURL
				}
				if r.PRURL != "" {
					r.Status, r.Reason = "merged", observedIssueMergeReason
					return finish()
				}
			}
			r.Status = "failed"
			r.Reason = "Pull request merged externally during review"
		} else if o.Status == OutcomeChecksPending {
			r.PRURL, r.Status, r.Reason = o.PRURL, OutcomeChecksPending, o.Reason
			r.MergeHead = ""
			return finish()
		} else if o.Status == OutcomeReviewPending {
			r.PRURL, r.Status, r.Reason = o.PRURL, awaitingReview, o.Reason
			r.MergeHead = ""
			if r.Reason == "" {
				r.Reason = "Waiting for review to resume"
			}
		} else if o.Status == "failed" {
			r.Status = "failed"
			r.Reason = o.Reason
		} else if o.Status == "succeeded" {
			r.PRURL = o.PRURL
			if r.PRURL == "" {
				r.PRURL = r.Item.URL
			}
			if o.PendingReviewGate != nil {
				r.Status = awaitingReview
			} else if r.Workflow.Merge.Enabled && (!exists || w.Merge.Enabled) && strings.Contains(r.PRURL, "/pull/") {
				r.Status = "waiting"
				r.Reason = "Waiting for checks and merge requirements"

			} else {
				r.Status = "succeeded"
				r.Reason = "Task completed"
			}
		}
	}
	if r.Status == awaitingReview && r.hasPendingReview() {
		if !exists {
			r.Status, r.Reason = "completed", "Originating workflow removed before review resumed"
		} else if original.Status == awaitingReview {
			return nil
		}
		return finish()
	}

	if r.Status == awaitingReview && r.Outcome != nil && r.Outcome.PendingReviewGate != nil {
		if !exists {
			r.Status, r.Reason = "completed", "Originating workflow removed before review writeback"
			return finish()
		}
		if !active || !e.StillEnabled(r.Workflow) {
			r.Reason = "Review writeback paused: workflow changed or was disabled"
			return finish()
		}
		receipt := r.cleanReviewReceipt(r.Outcome)
		if receipt == nil || e.FinishReview == nil {
			r.Reason = "Waiting for a submitted clean review on the current pull request head"
			return finish()
		}
		i, err := e.InspectPR(ctx, r.Item.Repository, r.PRURL)
		if err != nil {
			waitForQuota(err)
			return errors.Join(err, finish())
		}
		if i.Merged && r.Item.Kind == "issue" {
			mergedPR = i
			r.Status, r.Reason = "merged", observedIssueMergeReason
			return finish()
		}
		if !i.Open {
			r.Status, r.Reason = "completed", "Pull request closed before review writeback"
			return finish()
		}
		if i.HeadSHA != receipt.Head {
			r.Reason = "Head changed after merge eligibility; a new review is required"
			return finish()
		}
		if !e.StillEnabled(r.Workflow) {
			r.Reason = "Review writeback paused: workflow changed or was disabled"
			return finish()
		}
		if err := commit(); err != nil || stale {
			return err
		}
		gate := *r.Outcome.PendingReviewGate
		gate.MarkReady = gate.MarkReady && i.Draft
		err = e.FinishReview(ctx, *receipt, &gate)
		outcome := *r.Outcome
		outcome.PendingReviewGate = &gate
		r.Outcome = &outcome
		switch {
		case errors.Is(err, githubutil.ErrReviewInterrupted) && r.Item.Kind == "issue":
			r.Status, r.Reason = "merged", observedIssueMergeReason
		case errors.Is(err, githubutil.ErrReviewInterrupted):
			r.Status, r.Reason = "completed", "Pull request was already merged when the workflow checked"
		case errors.Is(err, githubutil.ErrReviewThreadsPending):
			r.Reason = "Waiting for unresolved review threads"
		case errors.Is(err, githubutil.ErrReviewChangesRequested):
			r.Reason = "Waiting for requested changes"
		case waitForQuota(err):
		case err != nil:
			r.Reason = "Review writeback pending: " + err.Error()
		default:
			outcome.PendingReviewGate = nil
			if r.Workflow.Merge.Enabled && w.Merge.Enabled {
				r.Status, r.Reason = "waiting", "Waiting for checks and merge requirements"
			} else {
				r.Status, r.Reason = "succeeded", "Task completed"
			}
		}
		return finish()
	}
	waiting := r.Status == "waiting" || r.Status == awaitingReview
	if waiting && (!exists || !w.Merge.Enabled) {
		r.Status = "succeeded"
		r.Reason = "Automatic merge disabled in current definition"
		waiting = false
	}
	if waiting && !active {
		r.Reason = "Merge paused: workflow feature, workflow, or repository is disabled"
	}
	if waiting && active {
		appID, policyErr := githubutil.RequiredReviewCheckApp(r.Item.Repository)
		if policyErr != nil {
			r.Reason = policyErr.Error()
			return finish()
		}
		reviewRequired := r.requiresReview() || appID > 0
		i, inspectErr := e.InspectPR(ctx, r.Item.Repository, r.PRURL)
		if inspectErr != nil {
			waitForQuota(inspectErr)
			return errors.Join(inspectErr, finish())
		}
		if i.Open && reviewRequired {
			receipt := r.cleanReviewReceipt(r.Outcome)
			if receipt == nil {
				// Retain the activation for late evidence without reserving the
				// entity against fresh review work.
				r.Status = awaitingReview
				r.Reason = "Waiting for a submitted clean review on the current pull request head"
				return finish()
			}
			r.MergeHead = receipt.Head
		}
		r.Status = "waiting"
		if !reviewRequired && r.MergeHead == "" && i.HeadSHA != "" {
			r.MergeHead = i.HeadSHA
		}
		if err := commit(); err != nil {
			return err
		}
		if stale {
			return nil
		}
		if i.Merged && r.Item.Kind == "issue" {
			mergedPR = i
			r.Status, r.Reason = "merged", observedIssueMergeReason
		} else if i.Merged && reviewRequired {
			r.Status = "completed"
			r.Reason = "Pull request was already merged when the workflow checked"
		} else if i.Merged {
			r.Status = "merged"
			r.Reason = "Pull request merged"
		} else if !i.Open {
			r.Status = "completed"
			r.Reason = "Pull request closed"
		} else if reviewRequired && i.HeadSHA != "" && i.HeadSHA != r.MergeHead {
			r.Status = awaitingReview
			r.Reason = "Head changed after merge eligibility; a new review is required"
		} else if i.Draft {
			r.Reason = "Waiting for pull request to leave draft"
		} else if i.ReviewDecision == "CHANGES_REQUESTED" {
			r.Reason = "Waiting for requested changes"
		} else if !i.ChecksPassed {
			r.Reason = "Waiting for successful checks"
		} else if r.Workflow.Merge.RequireApproval && !i.HumanApproved {
			r.Reason = "Waiting for human approval"
		} else if i.HeadSHA == "" {
			r.Reason = "Waiting for pull request head commit"
		} else if r.MergeHead != i.HeadSHA {
			r.Status = "failed"
			r.Reason = "Head changed after merge eligibility; a new review is required"
		} else if !e.Allowed(r.Item.Repository) || !e.StillEnabled(r.Workflow) {
			r.Reason = "Workflow changed or was disabled before merge"
		} else if mergeErr := e.Merge(ctx, r); errors.Is(mergeErr, githubutil.ErrReviewInterrupted) {
			if r.Item.Kind == "issue" {
				r.Status, r.Reason = "merged", observedIssueMergeReason
			} else {
				r.Status, r.Reason = "completed", "Pull request was already merged when the workflow checked"
			}
		} else if errors.Is(mergeErr, githubutil.ErrReviewCheckPending) {
			r.Reason = "Waiting for the trusted review policy check on the current pull request head"
		} else if errors.Is(mergeErr, githubutil.ErrReviewThreadsPending) {
			r.Reason = "Waiting for unresolved review threads"
		} else if errors.Is(mergeErr, githubutil.ErrReviewChangesRequested) {
			r.Reason = "Waiting for requested changes"
		} else if errors.Is(mergeErr, githubutil.ErrReviewApprovalPending) {
			r.Reason = "Waiting for human approval"
		} else if mergeErr != nil {
			if !waitForQuota(mergeErr) {
				r.Reason = "Merge blocked: " + mergeErr.Error()
			}
		} else {
			mergedPR = Item{Title: i.Title}
			if r.Item.Kind == "issue" {
				// The pre-merge inspection cannot supply GitHub's merge timestamp.
				if observed, err := e.InspectPR(ctx, r.Item.Repository, r.PRURL); err == nil && observed.Merged {
					mergedPR.MergedAt = observed.MergedAt
					if observed.Title != "" {
						mergedPR.Title = observed.Title
					}
				}
				receipt := &appMergeReceipt{Title: mergedPR.Title, MergedAt: mergedPR.MergedAt}
				if err := e.transaction(func(s *State) error {
					current := s.run(r.ID)
					if current == nil {
						return ErrActivationNotFound
					}
					current.AppMerge = receipt
					return nil
				}); err != nil {
					return err
				}
			}
			mergedByApp = true
			r.Status = "merged"
			r.Reason = "Pull request merged"
		}
	}

	return finish()
}

func (r Run) requiresReview() bool {
	return r.Workflow.Task.Kind == "review" || r.ReviewPolicy().ReviewCycles > 0 || (r.Outcome != nil && r.Outcome.ReviewRequired)
}

func (r Run) cleanReviewReceipt(outcome *Outcome) *githubutil.ReviewReceipt {
	if outcome == nil || outcome.Status != "succeeded" || outcome.ReviewInterrupted || outcome.ReviewReceipt == nil {
		return nil
	}
	receipt := outcome.ReviewReceipt
	if !receipt.Clean() || receipt.PRURL != r.PRURL ||
		!strings.EqualFold(githubutil.PullRequestRepository(receipt.PRURL), r.Item.Repository) ||
		receipt.Policy.Hash != r.Workflow.ExecutionPolicyHash() {
		return nil
	}
	policy := r.ReviewPolicy()
	passes := policy.ReviewCycles
	if r.Workflow.Task.Kind == "review" {
		passes = 0
	}
	if receipt.Policy.FixPasses != passes || receipt.Pass > passes+1 ||
		(r.Execution != nil && receipt.Policy.Complexity != policy.ReviewAgent.Complexity) {
		return nil
	}
	return receipt
}

func terminal(status string) bool {
	return contains([]string{"failed", "succeeded", "merged", "completed"}, status)
}

// A recorded pending review can supersede a failed activation status.
func (r Run) hasFailedPendingReview() bool {
	return r.Status == "failed" && r.hasPendingReview()
}

func (r Run) hasPendingReview() bool {
	return r.Outcome != nil && r.Outcome.Status == OutcomeReviewPending && !r.Outcome.ReviewInterrupted
}

func (e *Engine) recordIssueMerge(r Run, prURL string, pr Item, mergedByApp bool) error {
	if e.RecordIssueMerge == nil {
		return nil
	}
	if mergedByApp || (r.Status != awaitingReview && r.Status != "waiting" && r.Status != OutcomeChecksPending && !r.hasFailedPendingReview()) {
		r.Reason = ""
		if !mergedByApp && r.Outcome != nil && r.Outcome.ReviewInterrupted && r.Outcome.Status == "failed" {
			r.Reason = r.Outcome.Reason
		}
	}
	pr.URL = prURL
	pr.Merged = true
	return e.RecordIssueMerge(r, pr, mergedByApp)
}

// ObserveIssueMerge releases the activation after the issue claim records the merge.
func (e *Engine) ObserveIssueMerge(id, requestID, prURL string) error {
	if id == "" || prURL == "" {
		return nil
	}
	// A tick can be publishing the app's merge receipt or waiting on issue
	// admission. Reconciliation retries this cleanup on its next pass.
	if !e.tickMu.TryLock() {
		return nil
	}
	defer e.tickMu.Unlock()
	var merged Run
	err := e.transaction(func(s *State) error {
		r := s.run(id)
		if r == nil || e.submitting[id] || r.Status == "merged" || (r.RequestID != "" && r.RequestID != requestID) ||
			!strings.HasPrefix(r.Workflow.Trigger.Event, "issue.") ||
			(r.LinkedPRURL() != "" && !strings.EqualFold(r.LinkedPRURL(), prURL)) {
			return errJournalUnchanged
		}
		pr, byApp := Item{}, r.AppMerge != nil
		reason := observedIssueMergeReason
		if byApp {
			pr = Item{Title: r.AppMerge.Title, MergedAt: r.AppMerge.MergedAt}
			reason = "Pull request merged"
		}
		if err := e.recordIssueMerge(*r, prURL, pr, byApp); err != nil {
			return err
		}
		r.Status, r.Reason, r.PRURL = "merged", reason, prURL
		r.RequestID, r.UpdatedAt, r.GitHubRetryAt = requestID, e.now(), time.Time{}
		merged = *r
		return nil
	})
	if err == nil && merged.ID != "" {
		e.logRun(merged, "transition")
	}
	return err
}

// PrepareIssueRecovery retains the original event and merge policy before an
// explicit recovery can enqueue a worker. Older journals dropped the run when
// admission failed, even though the issue store retained the event and policy.
func (e *Engine) PrepareIssueRecovery(workflowID, policy, eventID, retryOf string, item Item, execution *ExecutionPolicy) error {
	if eventID == "" || policy == "" {
		return fmt.Errorf("issue recovery requires a saved workflow event and policy")
	}
	doc, err := e.Store.Load()
	if err != nil {
		return err
	}
	for _, w := range doc.Workflows {
		if w.ID != workflowID || !strings.HasPrefix(w.Trigger.Event, "issue.") ||
			w.ExecutionPolicyHash() != policy || CanonicalRepository(w.Repository) != CanonicalRepository(item.Repository) ||
			!e.StillEnabled(w) {
			continue
		}
		return e.transaction(func(s *State) error {
			if existing := s.run(eventID); existing != nil {
				if existing.RetryOf != retryOf || existing.Status != "pending_issue" || existing.RequestID != "" || existing.Outcome != nil ||
					existing.Workflow.ID != workflowID || existing.Workflow.ExecutionPolicyHash() != policy ||
					CanonicalRepository(existing.Item.Repository) != CanonicalRepository(item.Repository) || existing.Item.Number != item.Number {
					return fmt.Errorf("workflow event already has an active or incompatible run")
				}
				return errJournalUnchanged
			}
			now := e.now()
			// Recovery can arrive before a tick consumes the recorded outcome.
			if previous := s.run(retryOf); previous != nil && previous.hasPendingReview() && !e.submitting[previous.ID] &&
				(previous.Status == awaitingReview || previous.Status == "running" || previous.Status == "failed") &&
				previous.Workflow.ID == workflowID && previous.Workflow.ExecutionPolicyHash() == policy &&
				CanonicalRepository(previous.Item.Repository) == CanonicalRepository(item.Repository) && previous.Item.Number == item.Number {
				previous.Status, previous.Reason, previous.UpdatedAt = "completed", "Review resumed by issue recovery", now
			}
			if s.busy(item) {
				return fmt.Errorf("issue already has an active workflow run")
			}
			s.Runs = append(s.Runs, Run{ID: eventID, RetryOf: retryOf, Workflow: w, Execution: execution, Item: item, Status: "pending_issue", Reason: "Waiting for issue claim recovery", StartedAt: &now, UpdatedAt: now})
			return nil
		})
	}
	return fmt.Errorf("recovery requires the originating workflow to be enabled with its original policy")
}

// Complete durably records the task's semantic outcome independently of UI logs.
func (e *Engine) Complete(id string, outcome Outcome) error {
	return e.transaction(func(s *State) error {
		r := s.run(id)
		if r == nil {
			return ErrActivationNotFound
		}
		if r.Status == OutcomeChecksPending || ((r.Status == awaitingReview || r.Status == "failed") && r.hasPendingReview()) {
			r.Status = "running"
		}
		r.Outcome = &outcome
		return nil
	})
}

// StillEnabled rechecks switches after potentially slow GitHub reads.
func (e *Engine) StillEnabled(w Definition) bool {
	if !e.Enabled() || !e.Allowed(w.Repository) {
		return false
	}
	doc, err := e.Store.Load()
	if err != nil {
		return false
	}
	for _, current := range doc.Workflows {
		if current.ID == w.ID && current.Enabled && strings.EqualFold(current.Repository, w.Repository) {
			return current.ExecutionPolicyHash() == w.ExecutionPolicyHash()
		}
	}
	return false
}
