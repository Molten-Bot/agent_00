package localruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

// IssueClaimRecord is a durable admission journal, not a work queue. An
// interrupted admission is never replayed automatically: GitHub may have
// accepted a request whose response was lost.
type IssueClaimRecord struct {
	ID         string        `json:"id"`
	InstanceID string        `json:"instanceId"`
	Phase      string        `json:"phase"`
	RequestID  string        `json:"requestId,omitempty"`
	Error      string        `json:"error,omitempty"`
	UpdatedAt  time.Time     `json:"updatedAt"`
	Run        config.Config `json:"run"`
}

type IssueClaimStore struct {
	Root string
	mu   sync.Mutex
}

var issueClaimIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (s *IssueClaimStore) Save(record IssueClaimRecord) error {
	if s == nil || s.Root == "" {
		return fmt.Errorf("issue claim storage is unavailable")
	}
	if !issueClaimIDPattern.MatchString(record.ID) {
		return fmt.Errorf("invalid issue claim ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return fmt.Errorf("create issue claim directory: %w", err)
	}
	record.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.Root, ".claim-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), filepath.Join(s.Root, record.ID+".json")); err != nil {
		return err
	}
	// Sync the directory so an acknowledged admission survives a machine restart.
	dir, err := os.Open(s.Root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *IssueClaimStore) List() ([]IssueClaimRecord, error) {
	if s == nil || s.Root == "" {
		return nil, fmt.Errorf("issue claim storage is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(s.Root, "*.json"))
	if err != nil {
		return nil, err
	}
	records := make([]IssueClaimRecord, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record IssueClaimRecord
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("decode issue claim: %w", err)
		}
		if !issueClaimIDPattern.MatchString(record.ID) || record.Run.GitHubIssue == nil || record.Run.GitHubIssue.Claim == nil || record.Run.GitHubIssue.Claim.ID != record.ID {
			return nil, fmt.Errorf("invalid persisted issue claim")
		}
		records = append(records, record)
	}
	return records, nil
}
