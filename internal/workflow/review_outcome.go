package workflow

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// ReviewOutcomePath locates the durable review evidence for a task request.
func ReviewOutcomePath(root, requestID string) string {
	return filepath.Join(root, "review-receipts", fmt.Sprintf("%x.json", sha256.Sum256([]byte(requestID))))
}

// SaveReviewOutcome keeps review evidence after the task workspace is cleaned
// and the workflow activation is retired.
func SaveReviewOutcome(root, requestID string, outcome Outcome) error {
	outcome.Reason = ""
	return writeAtomic(ReviewOutcomePath(root, requestID), outcome)
}

func LoadReviewOutcome(root, requestID string) (Outcome, error) {
	var outcome Outcome
	data, err := os.ReadFile(ReviewOutcomePath(root, requestID))
	if err != nil {
		return outcome, err
	}
	if err := decodeStrict(data, &outcome); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}
