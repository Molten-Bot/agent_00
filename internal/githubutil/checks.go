package githubutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

type StatusCheck struct {
	Conclusion, State, Name, Context string
}

// CommitCheckNames reads both kinds of checks independently of the PR rollup.
func CommitCheckNames(ctx context.Context, gh ReviewCommand, repo, head string) ([]string, error) {
	prefix := "repos/" + repo + "/commits/" + url.PathEscape(head)
	var names []string
	for _, source := range []struct{ endpoint, field string }{
		{"/check-runs", "check_runs"},
		{"/status", "statuses"},
	} {
		raw, err := gh(ctx, "api", "--paginate", prefix+source.endpoint+"?per_page=100")
		if err != nil {
			return nil, fmt.Errorf("load commit %s: %w", source.field, err)
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		for pageNumber := 0; ; pageNumber++ {
			var page map[string]json.RawMessage
			if err := dec.Decode(&page); err == io.EOF && pageNumber > 0 {
				break
			} else if err != nil {
				return nil, fmt.Errorf("decode commit %s: %w", source.field, err)
			}
			var entries []StatusCheck
			if err := json.Unmarshal(page[source.field], &entries); err != nil || entries == nil {
				return nil, fmt.Errorf("missing or invalid commit %s inventory", source.field)
			}
			for _, entry := range entries {
				name := entry.Name
				if name == "" {
					name = entry.Context
				}
				if name == "" {
					return nil, fmt.Errorf("unnamed entry in commit %s inventory", source.field)
				}
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// CIStatus keeps CI independent of the review check, which has its own gate.
func CIStatus(checks []StatusCheck) (passed, failed bool) {
	count := 0
	passed = true
	for _, check := range checks {
		if check.Name == ReviewCheckName || check.Context == ReviewCheckName {
			continue
		}
		count++
		state := check.Conclusion
		if state == "" {
			state = check.State
		}
		switch state {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED":
			passed, failed = false, true
		default:
			passed = false
		}
	}
	return passed && count > 0, failed
}

func VerifyReviewedHeadCI(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt) error {
	return verifyReviewedHeadCI(ctx, gh, receipt, false)
}

// VerifyReadyHeadCI verifies reported checks on the reviewed head. Repositories
// without checks can still mark a PR ready after the publication check stage.
func VerifyReadyHeadCI(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt) error {
	return verifyReviewedHeadCI(ctx, gh, receipt, true)
}

func verifyReviewedHeadCI(ctx context.Context, gh ReviewCommand, receipt ReviewReceipt, allowEmpty bool) error {
	raw, err := gh(ctx, "pr", "view", PullRequestSelector(receipt.PRURL), "--repo", PullRequestRepository(receipt.PRURL),
		"--json", "state,headRefOid,statusCheckRollup")
	if err != nil {
		return fmt.Errorf("verify checks on the reviewed head: %w", err)
	}
	var pr struct {
		State, HeadRefOID string
		StatusCheckRollup []StatusCheck
	}
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		return fmt.Errorf("decode pull request checks: %w", err)
	}
	if pr.State == "MERGED" {
		return ErrReviewInterrupted
	}
	if pr.State != "OPEN" {
		return fmt.Errorf("pull request is closed before verifying checks")
	}
	if pr.HeadRefOID != receipt.Head {
		return ErrReviewHeadChanged
	}
	if allowEmpty && pr.StatusCheckRollup != nil {
		// The trusted review check is handled by the review gate itself.
		reported := false
		for _, check := range pr.StatusCheckRollup {
			reported = reported || (check.Name != ReviewCheckName && check.Context != ReviewCheckName)
		}
		if !reported {
			return nil
		}
	}
	if passed, _ := CIStatus(pr.StatusCheckRollup); !passed {
		return fmt.Errorf("waiting for successful checks on the reviewed head")
	}
	return nil
}
