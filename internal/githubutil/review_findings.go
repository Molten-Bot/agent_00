package githubutil

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var nativeReviewMarker = regexp.MustCompile(`^<!-- agent00-review [0-9a-f]{64} -->\n`)

// NativeReviewFinding identifies an original inline finding. GitHub keeps this
// identity and its reviewed commit even after the checkout and activation end.
type NativeReviewFinding struct {
	ReviewID  string `json:"reviewId"`
	CommentID string `json:"commentId"`
	Head      string `json:"originalHead"`
	Path      string `json:"path"`
	Line      int    `json:"originalLine"`
	Body      string `json:"finding"`
}

func (f NativeReviewFinding) Fingerprint() string {
	data, _ := json.Marshal(f)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (t reviewThread) nativeFinding() (NativeReviewFinding, bool) {
	if *t.Resolved || t.Comments.TotalCount != 1 || len(t.Comments.Nodes) != 1 {
		return NativeReviewFinding{}, false
	}
	c := t.Comments.Nodes[0]
	r := c.Review
	if !c.ViewerDidAuthor || !r.ViewerDidAuthor || r.State != "COMMENTED" || r.SubmittedAt == "" ||
		!nativeReviewMarker.MatchString(r.Body) || c.ID == "" || r.ID == "" || r.Commit.OID == "" ||
		c.Path == "" || c.OriginalLine <= 0 || strings.TrimSpace(c.Body) == "" {
		return NativeReviewFinding{}, false
	}
	return NativeReviewFinding{
		ReviewID: r.ID, CommentID: c.ID, Head: r.Commit.OID,
		Path: c.Path, Line: c.OriginalLine, Body: c.Body,
	}, true
}

func OpenNativeReviewFindings(ctx context.Context, gh ReviewCommand, prURL, head string) (map[string]NativeReviewFinding, error) {
	threads, err := reviewThreads(ctx, gh, prURL, head, false)
	if err != nil {
		return nil, err
	}
	findings := map[string]NativeReviewFinding{}
	for _, thread := range threads {
		if finding, ok := thread.nativeFinding(); ok {
			findings[thread.ID] = finding
		}
	}
	return findings, nil
}

// ReconcileVerifiedReviewThreads uses the reviewer-confirmed fingerprints.
// A retry starts with a fresh read, so a lost acknowledgement never requires
// replaying a mutation GitHub has already applied.
func ReconcileVerifiedReviewThreads(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt) error {
	if len(receipt.VerifiedThreads) == 0 {
		return nil
	}
	if err := verifySubmittedCleanReview(ctx, gh, receipt, false); err != nil {
		return err
	}
	findings, err := OpenNativeReviewFindings(ctx, gh, receipt.PRURL, receipt.Head)
	if err != nil {
		return err
	}
	for id, fingerprint := range receipt.VerifiedThreads {
		if finding, ok := findings[id]; ok && finding.Head != receipt.Head && finding.Fingerprint() == fingerprint {
			if err := resolveReviewThread(ctx, gh, id); err != nil {
				return fmt.Errorf("resolve verified review finding: %w", err)
			}
		}
	}
	return nil
}
