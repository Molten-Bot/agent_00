package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
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
	BaseBranch     string   `json:"baseBranch"`
	HeadBranch     string   `json:"headBranch"`
	HeadSHA        string   `json:"headSha"`
	Draft          bool     `json:"draft"`
	LinkedIssue    bool     `json:"linkedIssue"`
	ReviewDecision string   `json:"reviewDecision"`
	ChecksFailed   bool     `json:"checksFailed"`
	ChecksPassed   bool     `json:"checksPassed"`
	Open           bool     `json:"open"`
}
type Decision struct {
	Item    Item   `json:"item"`
	Matches bool   `json:"matches"`
	Reason  string `json:"reason"`
}

func Match(w Definition, i Item) (bool, string) {
	if !i.Open {
		return false, "Item is closed"
	}
	if !strings.EqualFold(w.Repository, i.Repository) {
		return false, "Repository does not match"
	}
	kind := strings.Split(w.Trigger.Event, ".")[0]
	if i.Kind != kind {
		return false, "Item type does not match"
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
	Outcome   *Outcome   `json:"outcome,omitempty"`
	ID        string     `json:"id"`
	RetryOf   string     `json:"retryOf,omitempty"`
	Workflow  Definition `json:"workflow"`
	Item      Item       `json:"item"`
	RequestID string     `json:"requestId,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	PRURL     string     `json:"prUrl,omitempty"`
	MergeHead string     `json:"mergeHead,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
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
type Outcome struct {
	ReviewClean  bool   `json:"reviewClean"`
	ReviewedHead string `json:"reviewedHead,omitempty"`
	Status       string `json:"status"`
	PRURL        string `json:"prUrl,omitempty"`
	Reason       string `json:"reason,omitempty"`
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
	wakeOnce  sync.Once
	wake      chan struct{}
	mu        sync.Mutex
}

var ErrIssueAdmissionPending = errors.New("waiting for issue claim recovery")

var ErrActivationNotFound = errors.New("workflow activation not found")

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
		if !terminal(run.Status) {
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
	case "Review posted; no confirmed clean review, so merge was not attempted":
		reason = "review_not_clean"
	}
	if strings.HasPrefix(r.Reason, "Merge blocked:") {
		reason = "merge_blocked"
	}
	var durationMS int64
	if r.StartedAt != nil && (r.Status == "succeeded" || r.Status == "merged") {
		durationMS = r.UpdatedAt.Sub(*r.StartedAt).Milliseconds()
	}
	e.Logf("workflow event=%s workflow_id=%q run_id=%q request_id=%q status=%s reason=%s duration_ms=%d", event, r.Workflow.ID, r.ID, r.RequestID, r.Status, reason, durationMS)
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
	items, err := e.Fetch(ctx, w.Repository, needs)
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
			e.Logf("workflow status=error err=%q", err)
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
	e.mu.Lock()
	defer e.mu.Unlock()
	featureEnabled := e.Enabled()
	d, err := e.Store.Load()
	if err != nil {
		return err
	}
	s, err := e.load()
	if err != nil {
		return err
	}
	previous := make(map[string]Run, len(s.Runs))
	for _, r := range s.Runs {
		previous[r.ID] = r
	}
	save := func() error { return e.save(s) }
	definitions := map[string]Definition{}
	for _, w := range d.Workflows {
		definitions[w.ID] = w
	}
	busy := map[string]bool{}
	var errs []error
	for n := range s.Runs {
		r := &s.Runs[n]
		w, exists := definitions[r.Workflow.ID]
		if terminal(r.Status) {
			continue
		}
		active := featureEnabled && exists && w.Enabled && e.Allowed(w.Repository) && e.Allowed(r.Item.Repository)
		if r.Status == "dispatching" || r.Status == "pending_issue" {
			if r.RequestID == "" && e.Reconcile != nil {
				r.RequestID = e.Reconcile(r.ID)
			}
			if r.RequestID == "" && r.Outcome == nil {
				if r.Status == "dispatching" {
					r.Reason = "Submission outcome unknown after restart; retained to prevent duplicate work"
				}
				continue
			}
			if r.Status == "pending_issue" {
				r.Reason = "Issue claim recovered"
			}
			r.Status = "running"
		}
		if r.Status == "running" {
			var o Outcome
			if r.Outcome != nil {
				o = *r.Outcome
			} else {
				o = e.Status(r.RequestID)
			}
			if o.Status == "failed" {
				r.Status = "failed"
				r.Reason = o.Reason
			} else if o.Status == "succeeded" {
				r.PRURL = o.PRURL
				if r.PRURL == "" {
					r.PRURL = r.Item.URL
				}
				if r.Workflow.Merge.Enabled && (!exists || w.Merge.Enabled) && strings.Contains(r.PRURL, "/pull/") {
					r.Status = "waiting"
					r.Reason = "Waiting for checks and merge requirements"
					if r.Workflow.Task.Kind == "review" {
						if !o.ReviewClean || o.ReviewedHead == "" {
							r.Status = "completed"
							r.Reason = "Review posted; no confirmed clean review, so merge was not attempted"
						} else {
							r.MergeHead = o.ReviewedHead
						}
					}
				} else {
					r.Status = "succeeded"
					r.Reason = "Task completed"
				}
			}
		}
		if r.Status == "waiting" && (!exists || !w.Merge.Enabled) {
			r.Status = "succeeded"
			r.Reason = "Automatic merge disabled in current definition"
		}
		if r.Status == "waiting" && !active {
			r.Reason = "Merge paused: workflow feature, workflow, or repository is disabled"
		}
		if r.Status == "waiting" && active {
			i, inspectErr := e.InspectPR(ctx, r.Item.Repository, r.PRURL)
			if inspectErr != nil {
				errs = append(errs, inspectErr)
				continue
			}
			if r.MergeHead == "" && i.HeadSHA != "" {
				r.MergeHead = i.HeadSHA
				if err := save(); err != nil {
					return err
				}
			}
			if i.Merged {
				r.Status = "merged"
				r.Reason = "Pull request merged"
			} else if !i.Open {
				r.Status = "completed"
				r.Reason = "Pull request closed"
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
			} else {
				if r.MergeHead == "" {
					r.MergeHead = i.HeadSHA
					if err := save(); err != nil {
						return err
					}
				}
				if r.MergeHead != i.HeadSHA {
					r.Status = "failed"
					r.Reason = "Head changed after merge eligibility; a new review is required"
				} else if !e.StillEnabled(r.Workflow) {
					r.Reason = "Workflow changed or was disabled before merge"
				} else if mergeErr := e.Merge(ctx, *r); mergeErr != nil {
					r.Reason = "Merge blocked: " + mergeErr.Error()
				} else {
					r.Status = "merged"
					r.Reason = "Pull request merged"
				}
			}
		}
		r.UpdatedAt = e.now()
	}
	for _, r := range s.Runs {
		if !terminal(r.Status) {
			entity := fmt.Sprintf("%s#%d", strings.ToLower(r.Item.Repository), r.Item.Number)
			busy[entity] = true
		}
	}
	if err := save(); err != nil {
		return err
	}
	for _, r := range s.Runs {
		old := previous[r.ID]
		if r.Status != old.Status || r.Reason != old.Reason {
			e.logRun(r, "transition")
		}
	}
	type snapshot struct {
		items []Item
		err   error
	}
	needsByRepo := map[string]ScanNeeds{}
	for _, w := range d.Workflows {
		if w.Enabled && (strings.HasPrefix(w.Trigger.Event, "issue.") || strings.HasPrefix(w.Trigger.Event, "pr.")) {
			key := strings.ToLower(w.Repository)
			needs := needsByRepo[key]
			needs.include(w)
			needsByRepo[key] = needs
		}
	}
	cache := map[string]snapshot{}
	reportedErrors := map[string]bool{}
	for _, w := range d.Workflows {
		if !featureEnabled || !w.Enabled || !e.Allowed(w.Repository) {
			continue
		}
		if IsInputEvent(w.Trigger.Event) {
			continue
		}
		if w.Trigger.Event == "schedule" {
			if err := e.tickSchedule(ctx, w, &s, busy); err != nil {
				return err
			}
			continue
		}
		key := strings.ToLower(w.Repository)
		fetched, ok := cache[key]
		if !ok {
			fetched.items, fetched.err = e.Fetch(ctx, w.Repository, needsByRepo[key])
			cache[key] = fetched
		}
		kind := strings.Split(w.Trigger.Event, ".")[0]
		if !usableSnapshot(kind, fetched.err) {
			if !reportedErrors[key] {
				errs = append(errs, fetched.err)
				reportedErrors[key] = true
			}
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
			obsKey := baselineKey + ":" + entity
			seen[obsKey] = true
			old := s.Observations[obsKey]
			match, reason := Match(w, i)
			fingerprint := ""
			if match {
				fingerprint = w.Trigger.Event
			}
			if old.Fingerprint == fingerprint {
				continue
			}
			// Keep observing a match becoming false while work is active. Only
			// defer admission of a new match, otherwise a later false-to-true
			// transition is lost permanently when the current task finishes.
			if match && busy[entity] && (hadBaseline || w.IncludeBacklog) {
				continue
			}
			old.Fingerprint = fingerprint
			old.Generation++
			s.Observations[obsKey] = old
			if !match || (!hadBaseline && !w.IncludeBacklog) {
				continue
			}
			id := digest([]any{obsKey, fingerprint, old.Generation})
			now := e.now()
			r := Run{ID: id, Workflow: w, Item: i, Status: "dispatching", Reason: reason, StartedAt: &now, UpdatedAt: now}
			s.Runs = append(s.Runs, r)
			if err := save(); err != nil {
				return err
			}
			e.logRun(r, "dispatch")
			var request string
			var submitErr error
			if !e.StillEnabled(w) {
				submitErr = fmt.Errorf("workflow changed or was disabled before submission")
			} else {
				request, submitErr = e.Submit(ctx, r)
			}
			index := len(s.Runs) - 1
			if errors.Is(submitErr, ErrIssueAdmissionPending) {
				s.Runs[index].Status = "pending_issue"
				s.Runs[index].Reason = submitErr.Error()
				busy[entity] = true
				errs = append(errs, submitErr)
			} else if submitErr != nil {
				s.Runs[index].Status = "failed"
				s.Runs[index].Reason = "Submission failed: " + submitErr.Error()
			} else {
				s.Runs[index].Status = "running"
				s.Runs[index].RequestID = request
				busy[entity] = true
			}
			if err := save(); err != nil {
				return err
			}
			e.logRun(s.Runs[index], "submission")
		}
		// A successful fetch contains every open item. Closed items disappear
		// from it; record that non-match so reopening can activate again.
		for obsKey, old := range s.Observations {
			if strings.HasPrefix(obsKey, baselineKey+":") && !seen[obsKey] && old.Fingerprint != "" {
				old.Fingerprint = ""
				old.Generation++
				s.Observations[obsKey] = old
			}
		}
		s.Baselines[baselineKey] = true
	}
	if err := save(); err != nil {
		return err
	}
	return errors.Join(errs...)
}
func terminal(status string) bool {
	return contains([]string{"failed", "succeeded", "merged", "completed"}, status)
}

// Complete durably records the task's semantic outcome independently of UI logs.
func (e *Engine) Complete(id string, outcome Outcome) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.load()
	if err != nil {
		return err
	}
	for n := range s.Runs {
		if s.Runs[n].ID == id {
			s.Runs[n].Outcome = &outcome
			return e.save(s)
		}
	}
	return ErrActivationNotFound
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
