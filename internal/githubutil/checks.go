package githubutil

import (
	"context"
	"encoding/json"
	"fmt"
)

type StatusCheck struct {
	Conclusion, State, Name, Context string
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
	raw, err := gh(ctx, "pr", "view", PullRequestSelector(receipt.PRURL), "--repo", PullRequestRepository(receipt.PRURL),
		"--json", "state,headRefOid,statusCheckRollup")
	if err != nil {
		return fmt.Errorf("verify checks before merging: %w", err)
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
	if pr.State != "OPEN" || pr.HeadRefOID != receipt.Head {
		return fmt.Errorf("pull request is closed or head changed before verifying checks")
	}
	if passed, _ := CIStatus(pr.StatusCheckRollup); !passed {
		return fmt.Errorf("waiting for successful checks before merging")
	}
	return nil
}
