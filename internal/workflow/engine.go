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

type Run struct {
	Outcome   *Outcome   `json:"outcome,omitempty"`
	ID        string     `json:"id"`
	Workflow  Definition `json:"workflow"`
	Item      Item       `json:"item"`
	RequestID string     `json:"requestId,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	PRURL     string     `json:"prUrl,omitempty"`
	MergeHead string     `json:"mergeHead,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
}
type observation struct {
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
}
type State struct {
	Version      int                    `json:"version"`
	Observations map[string]observation `json:"observations"`
	Baselines    map[string]bool        `json:"baselines"`
	Runs         []Run                  `json:"runs"`
}
type Outcome struct {
	ReviewClean  bool   `json:"reviewClean"`
	ReviewedHead string `json:"reviewedHead,omitempty"`
	Status       string `json:"status"`
	PRURL        string `json:"prUrl,omitempty"`
	Reason       string `json:"reason,omitempty"`
}
type Engine struct {
	Store     *Store
	StatePath string
	Enabled   func() bool
	Allowed   func(string) bool
	Fetch     func(context.Context, string) ([]Item, error)
	Submit    func(context.Context, Run) (string, error)
	Status    func(string) Outcome
	Reconcile func(string) string
	InspectPR func(context.Context, string, string) (Item, error)
	Merge     func(context.Context, Run) error
	Logf      func(string, ...any)
	mu        sync.Mutex
}

func (e *Engine) load() (State, error) {
	s := State{Version: 1, Observations: map[string]observation{}, Baselines: map[string]bool{}, Runs: []Run{}}
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
	if s.Version != 1 || s.Observations == nil || s.Baselines == nil {
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
func (e *Engine) Preview(ctx context.Context, w Definition) ([]Decision, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	if !e.Allowed(w.Repository) {
		return nil, fmt.Errorf("repository is disabled in Settings")
	}
	items, err := e.Fetch(ctx, w.Repository)
	if err != nil {
		return nil, err
	}
	out := []Decision{}
	for _, i := range items {
		if i.Kind != strings.Split(w.Trigger.Event, ".")[0] {
			continue
		}
		ok, reason := Match(w, i)
		out = append(out, Decision{i, ok, reason})
	}
	return out, nil
}
func (e *Engine) Run(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		if err := e.Tick(ctx); err != nil && e.Logf != nil {
			e.Logf("workflow status=error err=%q", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func (e *Engine) Tick(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.Enabled() {
		return nil
	}
	d, err := e.Store.Load()
	if err != nil {
		return err
	}
	s, err := e.load()
	if err != nil {
		return err
	}
	save := func() error { return writeAtomic(e.StatePath, s) }
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
		entity := fmt.Sprintf("%s#%d", strings.ToLower(r.Item.Repository), r.Item.Number)
		busy[entity] = true
		if !exists || !w.Enabled || (!e.Allowed(w.Repository) || !e.Allowed(r.Item.Repository)) {
			continue
		}
		if r.Status == "dispatching" {
			if e.Reconcile != nil {
				r.RequestID = e.Reconcile(r.ID)
			}
			if r.RequestID == "" {
				r.Reason = "Submission outcome unknown after restart; retained to prevent duplicate work"
				continue
			}
			r.Status = "running"
		}
		if r.Status == "running" {
			o := e.Status(r.RequestID)
			if r.Outcome != nil {
				o = *r.Outcome
			}
			if o.Status == "failed" {
				r.Status = "failed"
				r.Reason = o.Reason
			} else if o.Status == "succeeded" {
				r.PRURL = o.PRURL
				if r.PRURL == "" {
					r.PRURL = r.Item.URL
				}
				if r.Workflow.Merge.Enabled && w.Merge.Enabled && strings.Contains(r.PRURL, "/pull/") {
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
		if r.Status == "waiting" && !w.Merge.Enabled {
			r.Status = "succeeded"
			r.Reason = "Automatic merge disabled in current definition"
		}
		if r.Status == "waiting" {
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
			if !i.Open {
				r.Status = "completed"
				r.Reason = "Pull request closed"
			} else if i.Draft {
				r.Reason = "Waiting for pull request to leave draft"
			} else if i.ReviewDecision == "CHANGES_REQUESTED" {
				r.Reason = "Waiting for requested changes"
			} else if !i.ChecksPassed {
				r.Reason = "Waiting for successful checks"
			} else if r.Workflow.Merge.RequireApproval && i.ReviewDecision != "APPROVED" {
				r.Reason = "Waiting for human approval"
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
				} else if !e.StillEnabled(w) {
					r.Reason = "Workflow changed or was disabled before merge"
				} else if mergeErr := e.Merge(ctx, *r); mergeErr != nil {
					r.Reason = "Merge blocked: " + mergeErr.Error()
				} else {
					r.Status = "merged"
					r.Reason = "Pull request merged"
				}
			}
		}
		r.UpdatedAt = time.Now().UTC()
		if terminal(r.Status) {
			delete(busy, entity)
		}
	}
	if err := save(); err != nil {
		return err
	}
	cache := map[string][]Item{}
	for _, w := range d.Workflows {
		if !w.Enabled || !e.Allowed(w.Repository) {
			continue
		}
		key := strings.ToLower(w.Repository)
		items, ok := cache[key]
		if !ok {
			items, err = e.Fetch(ctx, w.Repository)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			cache[key] = items
		}
		// Configuration edits establish a fresh baseline; layout/name edits do not.
		policy := w
		policy.Positions = nil
		policy.Name = ""
		policy.Enabled = false
		baselineKey := w.ID + ":" + digest(policy)
		hadBaseline := s.Baselines[baselineKey]
		for _, i := range items {
			entity := fmt.Sprintf("%s#%d", key, i.Number)
			obsKey := baselineKey + ":" + entity
			old := s.Observations[obsKey]
			match, reason := Match(w, i)
			fingerprint := ""
			if match {
				fingerprint = w.Trigger.Event
			}
			if old.Fingerprint == fingerprint {
				continue
			}
			if busy[entity] && (hadBaseline || w.IncludeBacklog) {
				continue
			}
			old.Fingerprint = fingerprint
			old.Generation++
			s.Observations[obsKey] = old
			if !match || (!hadBaseline && !w.IncludeBacklog) {
				continue
			}
			id := digest([]any{obsKey, fingerprint, old.Generation})
			r := Run{ID: id, Workflow: w, Item: i, Status: "dispatching", Reason: reason, UpdatedAt: time.Now().UTC()}
			s.Runs = append(s.Runs, r)
			if err := save(); err != nil {
				return err
			}
			var request string
			var submitErr error
			if !e.StillEnabled(w) {
				submitErr = fmt.Errorf("workflow changed or was disabled before submission")
			} else {
				request, submitErr = e.Submit(ctx, r)
			}
			index := len(s.Runs) - 1
			if submitErr != nil {
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
			if s.Runs[n].Status == "failed" {
				s.Runs[n].Status = "running"
			}
			return writeAtomic(e.StatePath, s)
		}
	}
	return fmt.Errorf("workflow activation not found")
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
			a, b := current, w
			a.Name = ""
			b.Name = ""
			a.Positions = nil
			b.Positions = nil
			return digest(a) == digest(b)
		}
	}
	return false
}
