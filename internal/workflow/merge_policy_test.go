package workflow

import (
	"context"
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
				if merged != 1 || runs[0].Status != "merged" {
					t.Fatalf("display-only edit prevented merge: %+v", runs[0])
				}
			} else if runs[0].Status != "waiting" || runs[0].Reason != "Workflow changed or was disabled before merge" {
				t.Fatalf("missing policy-change diagnostic: %+v", runs[0])
			}
		})
	}
}
