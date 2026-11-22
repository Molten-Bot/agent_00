package localruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

type IssueRepairRecord struct {
	ID            string        `json:"id"`
	RequestID     string        `json:"requestId"`
	SourceClaimID string        `json:"sourceClaimId,omitempty"`
	State         string        `json:"state"`
	Error         string        `json:"error,omitempty"`
	PRURL         string        `json:"prUrl,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	NextAttemptAt time.Time     `json:"nextAttemptAt,omitempty"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	Started       bool          `json:"started,omitempty"`
	Run           config.Config `json:"run"`
}

type IssueRepairStore struct {
	Root string
	mu   sync.Mutex
}

func (s *IssueRepairStore) Save(record IssueRepairRecord) error {
	if !issueClaimIDPattern.MatchString(record.ID) || s.Root == "" {
		return fmt.Errorf("invalid issue repair record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record.UpdatedAt = time.Now().UTC()
	return writeIssueJSON(s.Root, record.ID+".json", record)
}

func (s *IssueRepairStore) List() ([]IssueRepairRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(s.Root, "*.json"))
	if err != nil {
		return nil, err
	}
	var records []IssueRepairRecord
	var problems []error
	for _, path := range paths {
		body, err := os.ReadFile(path)
		var record IssueRepairRecord
		if err == nil {
			err = json.Unmarshal(body, &record)
		}
		if err == nil && (!issueClaimIDPattern.MatchString(record.ID) || filepath.Base(path) != record.ID+".json") {
			err = fmt.Errorf("invalid repair identity")
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("repair %s: %w", filepath.Base(path), err))
			continue
		}
		records = append(records, record)
	}
	return records, errors.Join(problems...)
}
