package localruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func TestBeginExecutionReleasesReserveWhenReceiptSaveFails(t *testing.T) {
	t.Parallel()
	store := &IssueClaimStore{Root: t.TempDir()}
	id := strings.Repeat("a", 32)
	// A directory at the receipt path lets reserve creation succeed but
	// prevents the temporary receipt from being renamed into place.
	receipt := filepath.Join(store.Root, id+".json")
	var renameErr *os.LinkError
	err := store.beginExecution(IssueClaimRecord{ID: id}, func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if err := os.Mkdir(receipt, 0700); err != nil {
			t.Fatal(err)
		}
		return os.OpenFile(path, flags, mode)
	})
	if !errors.As(err, &renameErr) || renameErr.New != receipt {
		t.Fatalf("BeginExecution error = %v, want receipt rename failure", err)
	}
	if _, err := os.Stat(issueStorageReserve(store.Root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat execution reserve = %v, want missing reserve", err)
	}
}

func TestBeginExecutionDiskFullHoldsIssueWithoutChargingRetry(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"journal writable", "hold receipt write fails"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &IssueClaimStore{Root: t.TempDir()}
			id := strings.Repeat("a", 32)
			record := IssueClaimRecord{
				Version: 2, ID: id, State: "queued", LabelAdded: true,
				AgentAttempts: 1, Failures: 2, PriorRunFailures: 3, NextAttemptAt: time.Now().Add(time.Hour),
				Run: config.Config{GitHubIssue: &config.GitHubIssueConfig{Claim: &config.GitHubIssueClaim{ID: id}}},
			}
			if err := store.Save(record); err != nil {
				t.Fatal(err)
			}
			receipt := filepath.Join(store.Root, id+".json")
			err := store.beginExecution(record, func(path string, _ int, _ os.FileMode) (*os.File, error) {
				if name == "hold receipt write fails" {
					// Force the hold write to fail after reading the current claim.
					if err := os.Rename(receipt, receipt+".backup"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(receipt, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ENOSPC}
			})
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("BeginExecution error = %v, want ENOSPC", err)
			}
			records, err := store.List()
			if err != nil || len(records) != 1 || records[0].Incident != IssueIncidentWorkspaceStorage {
				t.Fatalf("queue records = %+v, error = %v", records, err)
			}
			if name == "hold receipt write fails" {
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(receipt+".backup", receipt); err != nil {
					t.Fatal(err)
				}
				if err := store.Update(id, func(r *IssueClaimRecord) error {
					if !r.StoppedAt.After(record.StoppedAt) || r.Incident != IssueIncidentWorkspaceStorage {
						t.Fatalf("pending hold lost before receipt could be saved: %+v", r)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			restarted := &IssueClaimStore{Root: store.Root}
			got, err := restarted.Load(id)
			if err != nil || got.State != "blocked" || got.Outcome != "error" || got.Incident != IssueIncidentWorkspaceStorage ||
				!got.UnlockPending || got.StoppedAt.IsZero() || !got.NextAttemptAt.IsZero() || got.ExecutionAgentAttempts != nil ||
				got.AgentAttempts != record.AgentAttempts || got.Failures != record.Failures || got.PriorRunFailures != record.PriorRunFailures ||
				!strings.Contains(got.Error, "select Retry") || !strings.Contains(got.FailureDetail(), syscall.ENOSPC.Error()) {
				t.Fatalf("restarted receipt = %+v, error = %v", got, err)
			}
		})
	}
}

func TestBeginExecutionRejectsStaleSnapshotAfterMerge(t *testing.T) {
	t.Parallel()
	for _, state := range []string{IssueStateMerged, IssueStateMergedUnverified} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			store := &IssueClaimStore{Root: t.TempDir()}
			id := strings.Repeat("a", 32)
			queued := IssueClaimRecord{ID: id, State: "queued",
				Run: config.Config{GitHubIssue: &config.GitHubIssueConfig{Claim: &config.GitHubIssueClaim{ID: id}}}}
			merged := queued
			merged.State, merged.PRMergedAt = state, time.Now().UTC()
			if err := store.Save(merged); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginExecution(queued); !errors.Is(err, ErrIssuePRMerged) {
				t.Fatalf("stale reservation returned %v, want ErrIssuePRMerged", err)
			}
			got, err := store.Load(id)
			if err != nil || got.State != state || !got.PRMergedAt.Equal(merged.PRMergedAt) {
				t.Fatalf("terminal receipt overwritten: %+v, %v", got, err)
			}
			if _, err := os.Stat(issueStorageReserve(store.Root, id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat execution reserve = %v, want missing reserve", err)
			}
		})
	}
}

func TestIssueClaimStorePersistsAdmissionWithoutPartialFiles(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "claims")
	store := &IssueClaimStore{Root: root}
	id := strings.Repeat("a", 32)
	record := IssueClaimRecord{ID: id, InstanceID: "instance", Phase: "reserving", Run: config.Config{GitHubIssue: &config.GitHubIssueConfig{Repository: "acme/repo", Number: 17, Claim: &config.GitHubIssueClaim{ID: id, Branch: "agent00-issue-17", BaseBranch: "main", ExclusionLabel: DefaultGitHubIssueExclusionLabel, Attempt: 1}}}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	record.Phase = "submitted"
	record.RequestID = "request-1"
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	restarted := &IssueClaimStore{Root: root}
	records, err := restarted.List()
	if err != nil || len(records) != 1 || records[0].RequestID != "request-1" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	info, err := os.Stat(filepath.Join(root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("claim permissions=%v", info.Mode())
	}
	record.ID = "../escape"
	if err := store.Save(record); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	if err := os.WriteFile(filepath.Join(root, id+".json"), []byte(`{"id":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("corrupt state silently ignored")
	}
}

func TestIssueAutomationSettingsReadErrorAndExclusionValidation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"github_issues_enabled":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfigUISettings(path); err == nil {
		t.Fatal("corrupt settings silently disabled automation")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	label := "claimed"
	labels := []string{"ready"}
	save := func() error {
		return SaveRuntimeConfigReviewDefaultLibraryAndGitHubIssuesSettings(path, InitConfig{}, ReviewWatchConfig{MergeMethod: "squash", ReviewLevel: "off"}, nil, nil, nil, nil, nil, nil, nil, RuntimeConfigUISettingsUpdate{Labels: &labels, ExclusionLabel: &label})
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadRuntimeConfigUISettings(path)
	if err != nil || settings.GitHubIssueAutoStartExclusionLabel != "claimed" {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
	labels = []string{"CLAIMED"}
	if err := save(); err == nil {
		t.Fatal("overlapping labels accepted")
	}
	settings, err = LoadRuntimeConfigUISettings(path)
	if err != nil || len(settings.GitHubIssueAutoStartLabels) != 1 || settings.GitHubIssueAutoStartLabels[0] != "ready" {
		t.Fatal("rejected update changed configuration")
	}
	labels = nil
	label = " "
	if err := save(); err == nil {
		t.Fatal("blank exclusion label accepted")
	}
}
