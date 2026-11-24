// Package workflow owns portable automation definitions and activation state.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

const Version = 2

type Document struct {
	LibrarySchedulesMigrated bool         `json:"librarySchedulesMigrated,omitempty"`
	Version                  int          `json:"version"`
	Revision                 uint64       `json:"revision"`
	Workflows                []Definition `json:"workflows"`
	Migration                string       `json:"migration,omitempty"`
}
type Definition struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Repository     string              `json:"repository"`
	Enabled        bool                `json:"enabled"`
	IncludeBacklog bool                `json:"includeBacklog"`
	Trigger        Trigger             `json:"trigger"`
	Filters        Filters             `json:"filters"`
	Task           Task                `json:"task"`
	ReviewCycles   int                 `json:"reviewCycles"`
	Merge          Merge               `json:"merge"`
	Positions      map[string]Position `json:"positions,omitempty"`
}
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type Trigger struct {
	IntervalHours float64    `json:"intervalHours,omitempty"`
	StartAt       *time.Time `json:"startAt,omitempty"`
	Event         string     `json:"event"`
	Value         string     `json:"value,omitempty"`
}
type Filters struct {
	Mode  string `json:"mode"`
	Rules []Rule `json:"rules"`
}
type Rule struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}
type Task struct {
	LibraryTaskName string    `json:"libraryTaskName,omitempty"`
	BaseBranch      string    `json:"baseBranch,omitempty"`
	TargetSubdir    string    `json:"targetSubdir,omitempty"`
	Reviewers       *[]string `json:"reviewers,omitempty"`
	Kind            string    `json:"kind"`
	Instructions    string    `json:"instructions"`
	AssignCreator   bool      `json:"assignCreator"`
	IssueLinkMode   string    `json:"issueLinkMode"`
	Draft           bool      `json:"draft"`
}
type Merge struct {
	Enabled         bool   `json:"enabled"`
	Method          string `json:"method"`
	RequireApproval bool   `json:"requireApproval"`
	DeleteBranch    bool   `json:"deleteBranch"`
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var Events = []string{"schedule", "app.prompt", "app.json", "app.chat", "issue.opened", "issue.assigned", "issue.labeled", "pr.opened", "pr.assigned", "pr.labeled", "pr.ready", "pr.review_requested", "pr.changes_requested", "pr.checks_failed"}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func (d Document) Validate() error {
	if d.Version != Version {
		return fmt.Errorf("unsupported workflow version %d (supported: %d); file was not modified", d.Version, Version)
	}
	ids := map[string]bool{}
	if len(d.Workflows) > 500 {
		return fmt.Errorf("at most 500 workflows are supported")
	}
	for _, w := range d.Workflows {
		if ids[w.ID] {
			return fmt.Errorf("duplicate workflow ID %q", w.ID)
		}
		ids[w.ID] = true
		if err := w.Validate(); err != nil {
			return fmt.Errorf("workflow %q: %w", w.Name, err)
		}
	}
	return nil
}
func (w Definition) Validate() error {
	if !idPattern.MatchString(w.ID) || strings.TrimSpace(w.Name) == "" || len(w.Name) > 100 {
		return fmt.Errorf("a stable ID and name (1–100 characters) are required")
	}
	if !repoPattern.MatchString(w.Repository) || strings.Contains(w.Repository, "..") {
		return fmt.Errorf("repository must be owner/name")
	}
	if !contains(Events, w.Trigger.Event) {
		return fmt.Errorf("unsupported trigger %q", w.Trigger.Event)
	}
	if (strings.HasSuffix(w.Trigger.Event, "assigned") || strings.HasSuffix(w.Trigger.Event, "labeled") || w.Trigger.Event == "pr.review_requested") && strings.TrimSpace(w.Trigger.Value) == "" {
		return fmt.Errorf("this trigger requires a label or GitHub login")
	}
	if w.Trigger.Event == "schedule" {
		minutes := w.Trigger.IntervalHours * 60
		if math.IsNaN(minutes) || math.IsInf(minutes, 0) || minutes < 1 || minutes > 525600 || math.Abs(minutes-math.Round(minutes)) > 1e-7 {
			return fmt.Errorf("scheduled interval must be whole minutes between 1 minute and 365 days")
		}
		if w.Task.Kind != "implement" || len(w.Filters.Rules) != 0 || w.IncludeBacklog {
			return fmt.Errorf("scheduled triggers run repository tasks without issue/PR filters or backlog")
		}
		if w.Trigger.StartAt != nil && w.Trigger.StartAt.IsZero() {
			return fmt.Errorf("startAt must be a valid timestamp")
		}
	} else if w.Trigger.IntervalHours != 0 || w.Trigger.StartAt != nil {
		return fmt.Errorf("schedule options require a scheduled trigger")
	}
	if IsInputEvent(w.Trigger.Event) && (w.Task.Kind != "implement" || w.Task.LibraryTaskName != "" || len(w.Filters.Rules) != 0 || w.IncludeBacklog || w.Trigger.Value != "") {
		return fmt.Errorf("app triggers use the submitted task with optional instructions, without library templates, GitHub filters, or backlog")
	}
	if w.Task.LibraryTaskName != "" && !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(w.Task.LibraryTaskName) {
		return fmt.Errorf("library task name must be a task slug")
	}
	if w.Filters.Mode != "all" && w.Filters.Mode != "any" {
		return fmt.Errorf("filter mode must be all or any")
	}
	if len(w.Filters.Rules) > 50 {
		return fmt.Errorf("at most 50 filter rules are supported")
	}
	for _, r := range w.Filters.Rules {
		if !contains([]string{"label", "author", "assignee", "reviewer", "issueType", "baseBranch", "draft", "bot", "linkedIssue"}, r.Field) || !contains([]string{"is", "isNot"}, r.Operator) || strings.TrimSpace(r.Value) == "" {
			return fmt.Errorf("invalid filter rule")
		}
		if contains([]string{"draft", "bot", "linkedIssue"}, r.Field) && r.Value != "true" && r.Value != "false" {
			return fmt.Errorf("%s requires true or false", r.Field)
		}
		if strings.HasPrefix(w.Trigger.Event, "issue.") && contains([]string{"reviewer", "baseBranch", "draft", "linkedIssue"}, r.Field) {
			return fmt.Errorf("%s is a pull request filter", r.Field)
		}
		if strings.HasPrefix(w.Trigger.Event, "pr.") && r.Field == "issueType" {
			return fmt.Errorf("issueType is an issue filter")
		}
	}
	if !contains([]string{"implement", "review", "repair"}, w.Task.Kind) || (strings.TrimSpace(w.Task.Instructions) == "" && w.Task.LibraryTaskName == "" && !IsInputEvent(w.Trigger.Event)) {
		return fmt.Errorf("task kind and instructions are required")
	}
	if strings.HasPrefix(w.Trigger.Event, "issue.") && w.Task.Kind != "implement" {
		return fmt.Errorf("issue triggers require an implementation task")
	}
	if strings.HasPrefix(w.Trigger.Event, "pr.") && w.Task.Kind == "implement" {
		return fmt.Errorf("PR triggers require review or repair")
	}
	if w.ReviewCycles < 0 || w.ReviewCycles > 10 {
		return fmt.Errorf("review cycles must be between 0 and 10")
	}
	if !contains([]string{"reference", "close"}, w.Task.IssueLinkMode) {
		return fmt.Errorf("issue link mode must be reference or close")
	}
	if !contains([]string{"squash", "merge", "rebase"}, w.Merge.Method) {
		return fmt.Errorf("merge method must be squash, merge, or rebase")
	}
	if w.Merge.Enabled && w.Task.Draft {
		return fmt.Errorf("draft pull requests cannot be automatically merged")
	}
	return nil
}
func Decode(data []byte) (Document, error) {
	var d Document
	err := decodeStrict(data, &d)
	if err == nil {
		if d.Version == 1 {
			d.Version = Version
		}
		err = d.Validate()
	}
	return d, err
}
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON document")
	}
	return nil
}
