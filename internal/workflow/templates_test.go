package workflow

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBundledTemplatesMatchWorkflowContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../web/workflow/src/templates.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version         int `json:"version"`
		WorkflowVersion int `json:"workflowVersion"`
		Templates       []struct {
			ID          string     `json:"id"`
			Name        string     `json:"name"`
			Description string     `json:"description"`
			Definition  Definition `json:"definition"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Version != 1 || catalog.WorkflowVersion != Version {
		t.Fatal("template schema version mismatch")
	}
	doc := Document{Version: catalog.WorkflowVersion}
	for _, template := range catalog.Templates {
		if template.ID != template.Definition.ID || template.Name == "" || template.Description == "" {
			t.Fatalf("invalid template metadata: %s", template.ID)
		}
		if template.Definition.Enabled || template.Definition.IncludeBacklog {
			t.Fatalf("template %s must require explicit activation", template.ID)
		}
		t.Run(template.ID, func(t *testing.T) {
			t.Parallel()
			e, items, count := testEngine(t, template.Definition)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			e.Now = func() time.Time { return now }
			tick(t, e)
			if *count != 0 {
				t.Fatal("disabled template dispatched")
			}
			doc, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			doc.Workflows[0].Enabled = true
			if _, err = e.Store.Save(doc); err != nil {
				t.Fatal(err)
			}
			tick(t, e)
			w := doc.Workflows[0]
			if w.Trigger.Event == "schedule" {
				now = now.Add(time.Duration(w.Trigger.IntervalHours) * time.Hour)
			} else {
				*items = []Item{{Repository: w.Repository, Number: 1, Kind: strings.Split(w.Trigger.Event, ".")[0], Open: true, Viewer: "agent", Labels: []string{w.Trigger.Value}, Assignees: []string{"agent"}, Reviewers: []string{"agent"}, ChecksFailed: true, ReviewDecision: "CHANGES_REQUESTED"}}
			}
			tick(t, e)
			tick(t, e)
			if *count != 1 {
				t.Fatalf("template dispatched %d tasks, want 1", *count)
			}
			runs, err := e.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Complete(runs[0].ID, Outcome{Status: "succeeded", PRURL: "https://github.com/example/repository/pull/2", ReviewClean: true, ReviewedHead: "abc"}); err != nil {
				t.Fatal(err)
			}
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				return Item{Open: true, HeadSHA: "abc", ChecksPassed: true, HumanApproved: true}, nil
			}
			merges := 0
			e.Merge = func(_ context.Context, r Run) error {
				merges++
				if r.Workflow.Merge != w.Merge || r.MergeHead != "abc" {
					t.Fatal("merge policy or head binding changed")
				}
				return nil
			}
			tick(t, e)
			tick(t, e)
			wantMerges := 0
			if w.Merge.Enabled {
				wantMerges = 1
			}
			if merges != wantMerges || *count != 1 {
				t.Fatalf("template completion: merges=%d tasks=%d", merges, *count)
			}
		})
		doc.Workflows = append(doc.Workflows, template.Definition)
	}
	if len(doc.Workflows) == 0 {
		t.Fatal("missing templates")
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
}
