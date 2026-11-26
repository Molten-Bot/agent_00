package githubutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func testReviewReceipt() ReviewReceipt {
	pr := "https://github.com/example/project/pull/12"
	return ReviewReceipt{
		PRURL: pr, Head: "aaa", Run: "run", Pass: 1, ReviewID: 50,
		Marker: "<!-- test review -->", URL: pr + "#pullrequestreview-50",
		Result: json.RawMessage(`{"status":"clean","mergeReady":true,"findings":[]}`),
	}
}

func TestVerifyReviewGateBlocksUnresolvedOrIncompleteThreads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, liveHead, reviewHead, decision, firstPage, secondPage, want string
	}{
		{name: "clean", firstPage: `"nodes":[],"pageInfo":{"hasNextPage":false}`},
		{name: "changed head", liveHead: "bbb", want: "head changed"},
		{name: "stale review", reviewHead: "bbb", want: "stale"},
		{name: "changes requested", decision: "CHANGES_REQUESTED", want: "requested changes"},
		{name: "old unresolved thread on page two",
			firstPage:  `"nodes":[{"id":"resolved","isResolved":true}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}`,
			secondPage: `"nodes":[{"id":"old","isResolved":false,"isOutdated":true}],"pageInfo":{"hasNextPage":false}`,
			want:       "unresolved review threads"},
		{name: "incomplete cursor", firstPage: `"nodes":[],"pageInfo":{"hasNextPage":true}`, want: "pagination"},
		{name: "missing visibility", firstPage: `"pageInfo":{"hasNextPage":false}`, want: "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testReviewReceipt()
			liveHead, reviewHead := tc.liveHead, tc.reviewHead
			if liveHead == "" {
				liveHead = r.Head
			}
			if reviewHead == "" {
				reviewHead = r.Head
			}
			review := SubmittedReview{ID: r.ReviewID, Head: reviewHead, URL: r.URL, State: "COMMENTED", SubmittedAt: "2026-11-25T12:00:00Z",
				Body: r.Marker + "\n" + CleanReviewVerdict + "\nNo material issues found."}
			pages := 0
			gh := func(_ context.Context, args ...string) (string, error) {
				switch {
				case args[0] == "pr":
					return fmt.Sprintf(`{"state":"OPEN","headRefOid":%q,"reviewDecision":%q}`, liveHead, tc.decision), nil
				case args[1] == "--paginate":
					data, _ := json.Marshal([]SubmittedReview{review})
					return "[]\n" + string(data), nil
				case args[1] == "graphql":
					pages++
					page := tc.firstPage
					if slices.Contains(args, "cursor=next") {
						page = tc.secondPage
					}
					return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":%q,"reviewThreads":{%s}}}}}`, liveHead, page), nil
				}
				return "", errors.New("unexpected command")
			}
			err := VerifyReviewGate(t.Context(), gh, r)
			if (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("VerifyReviewGate() = %v, want %q", err, tc.want)
			}
			if tc.secondPage != "" && pages != 2 {
				t.Fatalf("read %d thread pages, want 2", pages)
			}
		})
	}
}

func TestReconcileReviewThreadsRequiresUnchangedVerifiedAppFinding(t *testing.T) {
	t.Parallel()
	receipt := testReviewReceipt()
	marker := "<!-- agent00-review " + strings.Repeat("a", 64) + " -->\nFinding"
	thread := func(id, head, body string, byViewer bool, comments int) string {
		return fmt.Sprintf(`{"id":%q,"isResolved":false,"comments":{"totalCount":%d,"nodes":[{"id":%q,"body":%q,"path":"main.go","originalLine":5,"viewerDidAuthor":%t,"pullRequestReview":{"id":"review","body":%q,"state":"COMMENTED","submittedAt":"2026-11-25","viewerDidAuthor":%t,"commit":{"oid":%q}}}]}}`,
			id, comments, id+"-comment", body, byViewer, marker, byViewer, head)
	}
	editedBody := "original finding"
	var writes []string
	gh := func(_ context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "pr":
			return `{"state":"OPEN","headRefOid":"aaa","reviewDecision":null}`, nil
		case args[1] == "--paginate":
			review := SubmittedReview{ID: receipt.ReviewID, Head: receipt.Head, URL: receipt.URL, State: "COMMENTED", SubmittedAt: "2026-11-25",
				Body: receipt.Marker + "\n" + CleanReviewVerdict + "\nNo material issues found."}
			data, _ := json.Marshal([]SubmittedReview{review})
			return string(data), nil
		case strings.Contains(args[3], "resolveReviewThread("):
			id := strings.TrimPrefix(args[len(args)-1], "id=")
			writes = append(writes, id)
			return fmt.Sprintf(`{"data":{"resolveReviewThread":{"thread":{"id":%q,"isResolved":true}}}}`, id), nil
		default:
			nodes := []string{
				thread("verified", "old", "fixed finding", true, 1),
				thread("unverified", "old", "outstanding finding", true, 1),
				thread("edited", "old", editedBody, true, 1),
				thread("replied", "old", "finding with reply", true, 2),
				thread("human", "old", "human with copied marker", false, 1),
				thread("same-head", receipt.Head, "unrepaired finding", true, 1),
			}
			return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":%q,"reviewThreads":{"nodes":[%s],"pageInfo":{"hasNextPage":false}}}}}}`, receipt.Head, strings.Join(nodes, ",")), nil
		}
	}
	findings, err := OpenNativeReviewFindings(t.Context(), gh, receipt.PRURL, receipt.Head)
	if err != nil || len(findings) != 4 {
		t.Fatalf("open findings = %+v, %v", findings, err)
	}
	receipt.VerifiedThreads = map[string]string{
		"verified": findings["verified"].Fingerprint(), "edited": findings["edited"].Fingerprint(),
		"human": findings["human"].Fingerprint(), "replied": findings["replied"].Fingerprint(),
		"same-head": findings["same-head"].Fingerprint(),
	}
	editedBody = "edited since verification"
	if err := ReconcileVerifiedReviewThreads(t.Context(), gh, receipt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(writes, []string{"verified"}) {
		t.Fatalf("resolved threads = %v, want only verified", writes)
	}
	if err := VerifyReviewGate(t.Context(), gh, receipt); !errors.Is(err, ErrReviewThreadsPending) {
		t.Fatalf("unresolved findings did not block the gate: %v", err)
	}
}

func TestVerifyRequiredReviewCheckRequiresPinnedAppAndConversations(t *testing.T) {
	t.Parallel()
	const protected = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"agent00 / review-policy","integration_id":123}]}}]
[{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]`
	for _, tc := range []struct {
		raw     string
		appID   int64
		allowed bool
	}{
		{protected, 123, true},
		{protected, 124, false},
		{strings.ReplaceAll(protected, `,"integration_id":123`, ""), 123, false},
		{strings.ReplaceAll(protected, "true", "false"), 123, false},
		{protected + `[{"type":"merge_queue"}]`, 123, false},
		{protected + `[`, 123, false},
		{`[]`, 123, false},
	} {
		if err := verifyRequiredReviewCheck(tc.raw, tc.appID); (err == nil) != tc.allowed {
			t.Errorf("rules %s with app %d = %v, allowed %t", tc.raw, tc.appID, err, tc.allowed)
		}
	}
}

func TestHasPublishedReviewCheckMatchesHeadAppAndReceipt(t *testing.T) {
	t.Parallel()
	r := testReviewReceipt()
	const success = `{"check_runs":[{"id":123,"name":"agent00 / review-policy","head_sha":"aaa","external_id":"pr-12-review-50","status":"completed","conclusion":"success","app":{"id":456}}]}`
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{success, true},
		{strings.ReplaceAll(success, "aaa", "bbb"), false},
		{strings.ReplaceAll(success, "456", "457"), false},
		{strings.ReplaceAll(success, "pr-12", "pr-13"), false},
		{strings.ReplaceAll(success, "success", "neutral"), false},
	} {
		published, err := HasPublishedReviewCheck(t.Context(), func(_ context.Context, args ...string) (string, error) {
			if len(args) != 3 || !strings.Contains(args[2], "/commits/aaa/check-runs?") {
				t.Errorf("check query does not target the reviewed commit: %v", args)
			}
			return tc.raw, nil
		}, 456, r)
		if err != nil || published != tc.want {
			t.Fatalf("published = %t, err = %v for %s", published, err, tc.raw)
		}
	}
}

func TestPublishReviewCheckUsesAcknowledgedReceiptAndDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	receipt := testReviewReceipt()
	review := SubmittedReview{ID: receipt.ReviewID, Head: receipt.Head, URL: receipt.URL,
		State: "COMMENTED", SubmittedAt: "2026-11-25T12:00:00Z",
		Body: receipt.Marker + "\n" + CleanReviewVerdict + "\nNo material issues found."}
	reviews, err := json.Marshal([]SubmittedReview{review})
	if err != nil {
		t.Fatal(err)
	}
	const check = `{"id":123,"name":"agent00 / review-policy","head_sha":"aaa","external_id":"pr-12-review-50","status":"completed","conclusion":"success","app":{"id":456}}`
	writes := 0
	gh := func(_ context.Context, args ...string) (string, error) {
		switch joined := strings.Join(args, " "); {
		case strings.Contains(joined, "state,headRefOid,baseRefName"):
			return `{"state":"OPEN","headRefOid":"aaa","baseRefName":"release/main"}`, nil
		case strings.Contains(joined, "/rules/branches/release%2Fmain?"):
			return `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"agent00 / review-policy","integration_id":456}]}},{"type":"pull_request","parameters":{"required_review_thread_resolution":true}}]`, nil
		case strings.Contains(joined, "state,headRefOid,reviewDecision"):
			return `{"state":"OPEN","headRefOid":"aaa","reviewDecision":null}`, nil
		case strings.Contains(joined, "/reviews?"):
			return string(reviews), nil
		case strings.Contains(joined, "reviewThreads(first:100"):
			return `{"data":{"repository":{"pullRequest":{"state":"OPEN","headRefOid":"aaa","reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`, nil
		case strings.Contains(joined, "/commits/aaa/check-runs?"):
			if writes == 0 {
				return `{"check_runs":[]}`, nil
			}
			return `{"check_runs":[` + check + `]}`, nil
		case strings.HasPrefix(joined, "api --method POST repos/example/project/check-runs "):
			for _, field := range []string{"head_sha=" + receipt.Head, "external_id=pr-12-review-50", "details_url=" + receipt.EvidenceURL()} {
				if !slices.Contains(args, field) {
					t.Errorf("check write missing %s", field)
				}
			}
			writes++
			return check, nil
		default:
			return "", fmt.Errorf("unexpected GitHub command: %s", joined)
		}
	}
	if err := PublishReviewCheck(t.Context(), gh, 456, receipt); err != nil {
		t.Fatal(err)
	}
	if err := PublishReviewCheck(t.Context(), gh, 456, receipt); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("check writes = %d, want 1", writes)
	}
}
