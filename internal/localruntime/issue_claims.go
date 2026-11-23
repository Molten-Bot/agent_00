package localruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

// IssueClaimRecord is the durable lifecycle of one issue attempt. Version zero
// records predate worker locks and must never be automatically replayed.
type IssueClaimRecord struct {
	Version          int           `json:"version,omitempty"`
	ID               string        `json:"id"`
	InstanceID       string        `json:"instanceId"`
	Phase            string        `json:"phase"`
	RequestID        string        `json:"requestId,omitempty"`
	Error            string        `json:"error,omitempty"`
	UpdatedAt        time.Time     `json:"updatedAt"`
	Run              config.Config `json:"run"`
	State            string        `json:"state,omitempty"`
	Outcome          string        `json:"outcome,omitempty"`
	RunError         string        `json:"runError,omitempty"`
	ExitCode         int           `json:"exitCode,omitempty"`
	PublicationReady bool          `json:"publicationReady,omitempty"`
	Step             string        `json:"step,omitempty"`
	StepStartedAt    time.Time     `json:"stepStartedAt,omitempty"`
	StartedAt        time.Time     `json:"startedAt,omitempty"`
	StoppedAt        time.Time     `json:"stoppedAt,omitempty"`
	NextAttemptAt    time.Time     `json:"nextAttemptAt,omitempty"`
	AgentAttempts    int           `json:"agentAttempts,omitempty"`
	WorkspaceDir     string        `json:"workspaceDir,omitempty"`
	PRURL            string        `json:"prUrl,omitempty"`
	PRTitle          string        `json:"prTitle,omitempty"`
	VerifiedAt       time.Time     `json:"verifiedAt,omitempty"`
	LabelAdded       bool          `json:"labelAdded,omitempty"`
	UnlockPending    bool          `json:"unlockPending,omitempty"`
	NextUnlockAt     time.Time     `json:"nextUnlockAt,omitzero"`
	UnlockError      string        `json:"unlockError,omitempty"`
	UnlockFailures   int           `json:"unlockFailures,omitempty"`
	Incident         string        `json:"incident,omitempty"`
	RepairRequestID  string        `json:"repairRequestId,omitempty"`
	RepairError      string        `json:"repairError,omitempty"`
	Failures         int           `json:"failures,omitempty"`
	PriorRunFailures int           `json:"priorRunFailures,omitempty"`
	IssueNodeID      string        `json:"issueNodeId,omitempty"`
	ReservationSHA   string        `json:"reservationSha,omitempty"`
	RemoteRecorded   bool          `json:"remoteRecorded,omitempty"`
	BranchReserved   bool          `json:"branchReserved,omitempty"`
	ProcessGroups    []int         `json:"processGroups,omitempty"`
	ProcessUncertain bool          `json:"processUncertain,omitempty"`
	ProcessBootID    string        `json:"processBootId,omitempty"`
	NoChangeEvidence bool          `json:"noChangeEvidence,omitempty"`
	Control          string        `json:"control,omitempty"`
}

// FailureDetail prefers the terminal run error over a later recovery status.
func (r IssueClaimRecord) FailureDetail() string {
	if r.RunError != "" {
		return r.RunError
	}
	return r.Error
}

type IssueClaimStore struct {
	Root string
	mu   sync.Mutex
}

var issueClaimIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

var ErrIssueOwnershipChanged = errors.New("issue ownership changed; start the issue again to create a new attempt")

const (
	IssueIncidentUnmergedDependency = "unmerged_dependency"
	IssueIncidentOpenDependency     = "open_dependency"
	IssueIncidentExternalRollout    = "external_rollout_prerequisites"
)

// LegacyIssueOwnershipFailure is the error rendered by older binaries that wrapped a nil error.
const LegacyIssueOwnershipFailure = "issue owner is unavailable or changed: %!w(<nil>)"

func (s *IssueClaimStore) Save(record IssueClaimRecord) error {
	if s == nil || s.Root == "" {
		return fmt.Errorf("issue claim storage is unavailable")
	}
	if !issueClaimIDPattern.MatchString(record.ID) {
		return fmt.Errorf("invalid issue claim ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(record)
}

func (s *IssueClaimStore) save(record IssueClaimRecord) error {
	record.UpdatedAt = time.Now().UTC()
	return writeIssueJSON(s.Root, record.ID+".json", record)
}

func writeIssueJSON(root, name string, value any) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("create issue claim directory: %w", err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".claim-*")
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
	if err := os.Rename(file.Name(), filepath.Join(root, name)); err != nil {
		return err
	}
	// Sync the directory so an acknowledged admission survives a machine restart.
	dir, err := os.Open(root)
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
	var problems []error
	for _, path := range paths {
		record, err := readIssueRecord(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("issue record %s: %w", filepath.Base(path), err))
			continue
		}
		records = append(records, record)
	}
	return records, errors.Join(problems...)
}

func readIssueRecord(path string) (IssueClaimRecord, error) {
	var record IssueClaimRecord
	body, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(body, &record); err != nil {
		return record, err
	}
	if !issueClaimIDPattern.MatchString(record.ID) || filepath.Base(path) != record.ID+".json" || record.Run.GitHubIssue == nil || record.Run.GitHubIssue.Claim == nil || record.Run.GitHubIssue.Claim.ID != record.ID {
		return record, fmt.Errorf("invalid persisted issue claim")
	}
	return record, nil
}

func (s *IssueClaimStore) Load(id string) (IssueClaimRecord, error) {
	if s == nil || s.Root == "" || !issueClaimIDPattern.MatchString(id) {
		return IssueClaimRecord{}, fmt.Errorf("invalid issue storage or ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return readIssueRecord(filepath.Join(s.Root, id+".json"))
}

// Update prevents admission receipts from overwriting fast worker completion.
func (s *IssueClaimStore) Update(id string, change func(*IssueClaimRecord) error) error {
	if s == nil || s.Root == "" || !issueClaimIDPattern.MatchString(id) {
		return fmt.Errorf("invalid issue storage or ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := readIssueRecord(filepath.Join(s.Root, id+".json"))
	if err != nil {
		return err
	}
	if err := change(&record); err != nil {
		return err
	}
	return s.save(record)
}

// Identity survives restarts; callers hold the instance lock for the daemon's
// lifetime so two processes cannot dispatch from the same persistent directory.
func (s *IssueClaimStore) Identity() (string, error) {
	if s == nil || s.Root == "" {
		return "", fmt.Errorf("issue claim storage is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	body, err := os.ReadFile(filepath.Join(s.Root, "instance"))
	if err == nil {
		var id string
		if json.Unmarshal(body, &id) != nil || !issueClaimIDPattern.MatchString(id) {
			return "", fmt.Errorf("invalid persistent instance identity")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(bytes[:])
	return id, writeIssueJSON(s.Root, "instance", id)
}

func (s *IssueClaimStore) CheckOwner(instanceID string) error {
	owner, err := s.Identity()
	if err != nil {
		return fmt.Errorf("load issue owner: %w", err)
	}
	if owner != instanceID {
		return ErrIssueOwnershipChanged
	}
	return nil
}

// ScanPage checkpoints one page per repository per scan. A failed processing
// pass leaves the checkpoint unchanged, so observation cannot consume work.
func (s *IssueClaimStore) ScanPage(repository string, advanceTo int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pages := map[string]int{}
	body, err := os.ReadFile(filepath.Join(s.Root, "scan-pages"))
	if err == nil {
		if err := json.Unmarshal(body, &pages); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if advanceTo > 0 {
		pages[repository] = advanceTo
		return advanceTo, writeIssueJSON(s.Root, "scan-pages", pages)
	}
	return max(1, pages[repository]), nil
}
