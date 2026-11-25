package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/execx"
)

func TestGitHubPaginationWorksWithoutSlurp(t *testing.T) {
	t.Parallel()
	g := GitHub{Runner: fakeRunner(func(_ context.Context, c execx.Command) (execx.Result, error) {
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, "--slurp") {
			t.Fatal("requires unsupported --slurp flag")
		}
		if strings.Contains(args, "/issues?") {
			return execx.Result{Stdout: `[{"number":1,"title":"First"}]
[{"number":2,"title":"Second"}]`}, nil
		}
		return execx.Result{Stdout: "agent\n"}, nil
	})}
	items, err := g.Fetch(context.Background(), "acme/repo", ScanNeeds{Issues: true, PullRequests: true, Viewer: true, Reviews: true, LinkedIssues: true, Checks: true})
	if err != nil || len(items) != 2 || items[0].Number != 1 || items[1].Number != 2 {
		t.Fatal(items, err)
	}
	i := Item{HeadSHA: "abc"}
	err = applyReviews(&i, `[{"state":"APPROVED","commit_id":"abc","author_association":"MEMBER","user":{"login":"reviewer","type":"User"}}]
[{"state":"CHANGES_REQUESTED","commit_id":"abc","author_association":"MEMBER","user":{"login":"reviewer","type":"User"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if i.HumanApproved || i.ReviewDecision != "CHANGES_REQUESTED" {
		t.Fatal("later review page did not supersede approval")
	}
}

func TestGitHubPaginationRejectsIncompleteResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `[{"number":1}] invalid`, `[{"number":1}] null`, `[{"number":1}] [{`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			g := GitHub{Runner: fakeRunner(func(context.Context, execx.Command) (execx.Result, error) { return execx.Result{Stdout: raw}, nil })}
			items, err := g.Fetch(context.Background(), "acme/repo", ScanNeeds{Issues: true, PullRequests: true, Viewer: true, Reviews: true, LinkedIssues: true, Checks: true})
			if err == nil || items != nil {
				t.Fatalf("accepted incomplete response: %#v %v", items, err)
			}
			if err = applyReviews(&Item{}, raw); err == nil {
				t.Fatal("accepted incomplete review response")
			}
		})
	}
}

func TestGitHubInspectLinkedIssuesWithoutUnsupportedCLIField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		count           string
		linked, invalid bool
	}{{"0\n", false, false}, {"2\n", true, false}, {"null", false, true}, {"", false, true}, {"-1", false, true}} {
		t.Run(tc.count, func(t *testing.T) {
			t.Parallel()
			g := GitHub{Runner: fakeRunner(func(_ context.Context, c execx.Command) (execx.Result, error) {
				args := strings.Join(c.Args, " ")
				switch {
				case strings.HasPrefix(args, "pr view"):
					if strings.Contains(args, "closingIssuesReferences") {
						t.Fatal("requires unsupported gh JSON field")
					}
					return execx.Result{Stdout: `{"number":7,"state":"OPEN","headRefOid":"abc"}`}, nil
				case strings.HasPrefix(args, "api graphql"):
					if !strings.Contains(args, "number=7") || !strings.Contains(args, "owner=acme") || !strings.Contains(args, "name=repo") {
						t.Fatalf("wrong relationship query: %s", args)
					}
					return execx.Result{Stdout: tc.count}, nil
				default:
					return execx.Result{Stdout: `[]`}, nil
				}
			})}
			item, err := g.Inspect(context.Background(), "acme/repo", "7")
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v invalid=%v", err, tc.invalid)
			}
			if err == nil && item.LinkedIssue != tc.linked {
				t.Fatalf("linked=%v want=%v", item.LinkedIssue, tc.linked)
			}
		})
	}
}
