package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Molten-Bot/agent_00/internal/config"
	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

func TestIssueCacheReclaimUnblocksWorkspaceAdmission(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup_failure=%t", failure), func(t *testing.T) {
			t.Parallel()
			store, terminal := lifecycleRecord(t)
			root := filepath.Join(filepath.Dir(store.Root), "issue-workspaces")
			now := time.Now()
			cache := filepath.Join(workspace.AgentIODirName, "cache")
			npm := filepath.Join(cache, "npm", "_cacache", "content")
			goBuild := filepath.Join(cache, "go-build", "object")
			files := []string{
				npm, goBuild, filepath.Join(cache, "npm", "_logs", "npm.log"),
				filepath.Join(cache, "go-mod", "module.go"),
				filepath.Join("repo", "tracked.go"), "review.json", "task.log",
			}
			save := func(record localruntime.IssueClaimRecord) {
				t.Helper()
				if err := store.Save(record); err != nil {
					t.Fatal(err)
				}
			}
			seed := func(record localruntime.IssueClaimRecord) {
				t.Helper()
				save(record)
				for _, file := range files {
					path := filepath.Join(record.WorkspaceDir, file)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(file), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			terminal.State, terminal.Outcome, terminal.VerifiedAt = "fulfilled", "completed", now
			terminal.WorkspaceDir = filepath.Join(root, terminal.ID)
			seed(terminal)
			another := func(id string, number int) localruntime.IssueClaimRecord {
				record := terminal
				record.ID = strings.Repeat(id, 32)
				record.WorkspaceDir = filepath.Join(root, record.ID)
				record.Run.GitHubIssue = &config.GitHubIssueConfig{
					Repository: "acme/repo", Number: number, Claim: &config.GitHubIssueClaim{ID: record.ID},
				}
				record.VerifiedAt = now.Add(-time.Hour)
				return record
			}
			active := another("b", 18)
			seed(active)
			lock, err := store.TryLock(localruntime.IssueExecutionKey(active.Run.GitHubIssue))
			if err != nil || lock == nil {
				t.Fatalf("acquire active worker lock: %v", err)
			}
			defer lock.Close()

			retained := another("c", 19)
			seed(retained)
			retry := another("d", 19)
			retry.WorkspaceDir = retained.WorkspaceDir
			retry.State, retry.Outcome = localruntime.IssueChecksPending, localruntime.IssueChecksPending
			save(retry)

			outside := t.TempDir()
			symlinked := another("e", 20)
			save(symlinked)
			if err := os.Symlink(retained.WorkspaceDir, symlinked.WorkspaceDir); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "keep")
			if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(terminal.WorkspaceDir, filepath.Dir(npm), "outside")); err != nil {
				t.Fatal(err)
			}

			const unit = workspace.MinimumFreeBytes
			var free atomic.Uint64
			free.Store(unit)
			diskSpace := func(string) (string, uint64, uint64, error) { return "disk", free.Load(), 6 * unit, nil }
			logs := make(chan string, 32)
			logf := func(format string, args ...any) { logs <- fmt.Sprintf(format, args...) }
			cleanup := issueCacheReclaimer{
				claims: store, diskSpace: diskSpace, logf: logf,
				remove: func(root *os.Root, name string) error {
					if failure {
						return os.ErrPermission
					}
					if err := root.RemoveAll(name); err != nil {
						return err
					}
					free.Add(unit)
					return nil
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			controller := localruntime.NewAdaptiveScheduler(localruntime.SchedulerConfig{MaxParallel: 1}, logf,
				localruntime.WithSchedulerDiskSpace(diskSpace),
				localruntime.WithSchedulerDiskReclaim(func(path, device string, target uint64) {
					cleanup.reclaim(ctx, path, device, target)
				}),
			)
			next := another("f", 21)
			nextCtx := context.WithValue(ctx, issueExecutionContextKey{}, &issueExecution{record: next})
			key := localruntime.TaskDiskKey(terminal.Run.RepoList())
			history, err := localruntime.OpenTaskDiskHistory("")
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Record(key, 3*unit); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, release, err := controller.AcquireWorkspace(ctx, "next-issue", localTaskDiskRequest(nextCtx, history, key), false)
				if err == nil {
					release()
				}
				done <- err
			}()
			var queued, reclaimed bool
			wantReservation := fmt.Sprintf("required_bytes=%d reserved_bytes=0", 3*unit)
			for !queued || !reclaimed {
				select {
				case line := <-logs:
					if !queued && strings.Contains(line, "reason=disk_space") {
						queued = strings.Contains(line, "request_id=next-issue") && strings.Contains(line, wantReservation)
					}
					if strings.Contains(line, "action=reclaim_issue_caches") {
						reclaimed = true
						if failure && (!strings.Contains(line, "status=warn") || !strings.Contains(line, "permission denied")) {
							t.Fatalf("missing cleanup failure: %s", line)
						}
						if !failure && !strings.Contains(line, fmt.Sprintf("reclaimed_bytes=%d", 2*unit)) {
							t.Fatalf("missing reclaimed byte count: %s", line)
						}
					}
				case <-ctx.Done():
					t.Fatalf("missing queue or reclaim log; queue=%t reclaim=%t", queued, reclaimed)
				}
			}
			if failure {
				select {
				case err := <-done:
					t.Fatalf("task left the disk queue after cleanup failed: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				// Space freed externally must admit the same waiting task.
				free.Add(2 * unit)
				controller.RecheckQueue()
			}
			if err := <-done; err != nil {
				t.Fatalf("admission = %v, cleanup failure = %t", err, failure)
			}
			for _, record := range []localruntime.IssueClaimRecord{terminal, active, retained} {
				for _, file := range files {
					path := filepath.Join(record.WorkspaceDir, file)
					body, err := os.ReadFile(path)
					if !failure && record.ID == terminal.ID && (file == npm || file == goBuild) {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("terminal cache remains at %s: %v", file, err)
						}
					} else if err != nil || string(body) != file {
						t.Fatalf("claim %s lost %s: %v", record.ID, file, err)
					}
				}
			}
			if body, err := os.ReadFile(sentinel); err != nil || string(body) != "outside" {
				t.Fatalf("cleanup touched nested symlink target: %v", err)
			}
		})
	}
}
