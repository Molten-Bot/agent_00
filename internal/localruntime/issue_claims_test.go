package localruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Molten-Bot/agent_00/internal/config"
)

func TestBeginExecutionReleasesReserveWhenReceiptSaveFails(t *testing.T) {
	t.Parallel()
	store := &IssueClaimStore{Root: t.TempDir()}
	id := strings.Repeat("a", 32)
	// A directory at the receipt path lets reserve creation succeed but
	// prevents the temporary receipt from being renamed into place.
	receipt := filepath.Join(store.Root, id+".json")
	if err := os.Mkdir(receipt, 0700); err != nil {
		t.Fatal(err)
	}
	var renameErr *os.LinkError
	if err := store.BeginExecution(IssueClaimRecord{ID: id}); !errors.As(err, &renameErr) || renameErr.New != receipt {
		t.Fatalf("BeginExecution error = %v, want receipt rename failure", err)
	}
	if _, err := os.Stat(issueStorageReserve(store.Root, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat execution reserve = %v, want missing reserve", err)
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
