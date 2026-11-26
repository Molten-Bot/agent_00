package workflow

import (
	"context"
	"github.com/Molten-Bot/agent_00/internal/config"
	"testing"
)

func TestPendingMergeRejectsChangedWorkflowPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		edit      func(*Definition)
		wantMerge bool
	}{
		{"require approval", func(w *Definition) { w.Merge.RequireApproval = true }, false},
		{"merge method", func(w *Definition) { w.Merge.Method = "rebase" }, false},
		{"repository", func(w *Definition) { w.Repository = "acme/other" }, false},
		{"display name", func(w *Definition) { w.Name = "Renamed workflow" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := definition("policy-change")
			w.IncludeBacklog = true
			w.Merge.Enabled = true
			w.Merge.RequireApproval = false
			e, items, _ := testEngine(t, w)
			*items = []Item{{Repository: w.Repository, Number: 1, Kind: "issue", Open: true, Labels: []string{"ready"}}}
			tick(t, e)
			runs, err := e.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Complete(runs[0].ID, Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/2"}); err != nil {
				t.Fatal(err)
			}
			doc, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&doc.Workflows[0])
			if _, err := e.Store.Save(doc); err != nil {
				t.Fatal(err)
			}
			e.InspectPR = func(context.Context, string, string) (Item, error) {
				return Item{Open: true, ChecksPassed: true, HeadSHA: "abc"}, nil
			}
			merged := 0
			e.Merge = func(context.Context, Run) error { merged++; return nil }
			tick(t, e)
			if !tc.wantMerge && merged != 0 {
				t.Fatal("merged using the stale workflow policy")
			}
			runs, err = e.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantMerge {
				if merged != 1 || len(runs) != 0 {
					t.Fatalf("expected merge and no retained activation: merged=%d runs=%+v", merged, runs)
				}
			} else if runs[0].Status != "waiting" || runs[0].Reason != "Workflow changed or was disabled before merge" {
				t.Fatalf("missing policy-change diagnostic: %+v", runs[0])
			}
		})
	}
}

func TestRetryAfterRetirementUsesOnlyMatchingMergePolicy(t *testing.T) {
	t.Parallel()
	w := definition("retry")
	w.Merge.Enabled = true
	w.Merge.RequireApproval = false
	e, _, _ := testEngine(t, w)
	item := Item{Repository: w.Repository, Number: 1}
	var activation string
	submit := func(run Run) (string, error) {
		activation = run.ID
		return "retry-task", nil
	}
	if _, err := e.DispatchRetry(t.Context(), w.ID, "old-policy", "old-activation", item, submit); err != nil || activation != "" {
		t.Fatalf("retry should use ordinary task execution when policy changed: id=%q err=%v", activation, err)
	}
	if _, err := e.DispatchRetry(t.Context(), w.ID, w.ExecutionPolicyHash(), "old-activation", item, submit); err != nil || activation == "" {
		t.Fatalf("retry should get an active receipt: id=%q err=%v", activation, err)
	}
	if err := e.Complete(activation, Outcome{Status: "succeeded", PRURL: "https://github.com/acme/repo/pull/2"}); err != nil {
		t.Fatal(err)
	}
	e.InspectPR = func(context.Context, string, string) (Item, error) {
		return Item{Open: true, ChecksPassed: true, HeadSHA: "abc"}, nil
	}
	merged := 0
	e.Merge = func(context.Context, Run) error { merged++; return nil }
	tick(t, e)
	runs, err := e.Runs()
	if err != nil || merged != 1 || len(runs) != 0 {
		t.Fatalf("retry should merge once and leave no activation: merged=%d runs=%v err=%v", merged, runs, err)
	}
}

func TestMetadataRetryOnlyAllowsRoutingCorrections(t *testing.T) {
	t.Parallel()
	for _, unrelated := range []bool{false, true} {
		w := definition("routing")
		w.Task.PRAssignee = "former"
		w.Task.PRLabels = []string{"old"}
		e, _, _ := testEngine(t, w)
		saved := config.WorkflowRunConfig{ID: w.ID, PolicyHash: w.ExecutionPolicyHash(), PRAssignee: w.Task.PRAssignee, PRLabels: w.Task.PRLabels}
		w.Task.PRAssignee = "replacement"
		w.Task.PRLabels = []string{"new"}
		if unrelated {
			w.Merge.RequireApproval = !w.Merge.RequireApproval
		}
		doc, err := e.Store.Load()
		if err != nil {
			t.Fatal(err)
		}
		doc.Workflows[0] = w
		if _, err := e.Store.Save(doc); err != nil {
			t.Fatal(err)
		}
		got, err := e.PRMetadataRetryPolicy(saved)
		if unrelated {
			if err == nil {
				t.Fatal("accepted unrelated merge policy edit")
			}
		} else if err != nil || got.PRAssignee != "replacement" || got.PRLabels[0] != "new" || got.PolicyHash != w.ExecutionPolicyHash() {
			t.Fatalf("routing correction = %+v, %v", got, err)
		}
	}
}
