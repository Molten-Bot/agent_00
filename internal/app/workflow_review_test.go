package app

import (
	"context"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestWorkflowReviewOutcomeSeparatesCompletionFromCleanReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output string
		clean        bool
	}{{"clean", `{"status":"clean","mergeReady":true,"findings":[]}`, true}, {"findings", `{"status":"findings","mergeReady":false,"findings":[{"severity":"high","title":"Regression"}]}`, false}, {"unstructured", "Review completed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reported := false
			h := Harness{OnReviewResult: func(clean bool, head string) {
				reported = true
				if clean != tc.clean || head != "reviewed-sha" {
					t.Fatalf("outcome clean=%v head=%s", clean, head)
				}
			}}
			repo := repoWorkspace{}
			cfg := config.Config{Review: &config.ReviewConfig{PRNumber: 12, Writeback: "off"}}
			prepared := &preparedReviewContext{Metadata: reviewPRMetadata{Number: 12, URL: "https://github.com/acme/repo/pull/12", HeadRefOID: "reviewed-sha"}}
			if err := h.completeReviewRun(context.Background(), cfg, &repo, prepared, execx.Result{Stdout: tc.output}, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if !reported {
				t.Fatal("missing semantic outcome")
			}
		})
	}
}
