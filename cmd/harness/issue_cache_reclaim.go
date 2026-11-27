package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Molten-Bot/agent_00/internal/localruntime"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

type issueCacheReclaimer struct {
	claims    *localruntime.IssueClaimStore
	diskSpace func(string) (string, uint64, uint64, error)
	remove    func(*os.Root, string) error
	logf      func(string, ...any)
}

func (r issueCacheReclaimer) reclaim(ctx context.Context, path, device string, targetFree uint64) {
	pool, before, _, err := r.diskSpace(path)
	if err != nil {
		r.logf("workspace_storage status=warn action=reclaim_issue_caches device=%q err=%q", device, err)
		return
	}
	if pool != device || before >= targetFree {
		return
	}
	free, err := r.reclaimCaches(ctx, path, device, before, targetFree)
	status := "ok"
	detail := ""
	if free < targetFree {
		status = "insufficient"
	}
	if err != nil {
		status = "warn"
		detail = fmt.Sprintf(" err=%q", err)
	}
	r.logf("workspace_storage status=%s action=reclaim_issue_caches device=%q free_bytes=%d reclaimed_bytes=%d target_free_bytes=%d%s",
		status, device, free, free-min(free, before), targetFree, detail)
}

func (r issueCacheReclaimer) reclaimCaches(ctx context.Context, path, device string, free, targetFree uint64) (uint64, error) {
	rootPath, err := filepath.Abs(filepath.Join(filepath.Dir(r.claims.Root), "issue-workspaces"))
	if err != nil {
		return free, err
	}
	info, err := os.Lstat(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return free, nil
	}
	if err != nil {
		return free, err
	}
	if !info.IsDir() {
		return free, fmt.Errorf("issue workspace root is not a directory")
	}
	pool, _, _, err := r.diskSpace(rootPath)
	if err != nil || pool != device {
		return free, err
	}
	rootPathResolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return free, err
	}
	root, err := os.OpenRoot(rootPathResolved)
	if err != nil {
		return free, err
	}
	defer root.Close()
	records, err := r.claims.List()
	if err != nil {
		return free, err
	}
	slices.SortStableFunc(records, func(a, b localruntime.IssueClaimRecord) int {
		return a.VerifiedAt.Compare(b.VerifiedAt)
	})
	var problems []error
	visited := make(map[string]bool)
	for _, record := range records {
		if ctx.Err() != nil || free >= targetFree {
			break
		}
		if !canReclaimIssueCaches(record) {
			continue
		}
		dir, err := filepath.Abs(record.WorkspaceDir)
		if err != nil || filepath.Dir(dir) != rootPath || visited[dir] {
			continue
		}
		name := filepath.Base(dir)
		id, err := hex.DecodeString(name)
		if err != nil || len(id) != 16 {
			continue
		}
		visited[dir] = true
		free, err = r.reclaimWorkspace(ctx, root, name, dir, path, device, free, targetFree, record)
		if err != nil {
			problems = append(problems, fmt.Errorf("claim %s: %w", record.ID, err))
		}
	}
	return free, errors.Join(problems...)
}

func canReclaimIssueCaches(record localruntime.IssueClaimRecord) bool {
	return record.Version == 2 && record.Fulfilled() && !record.VerifiedAt.IsZero() &&
		record.WorkspaceDir != "" && record.ExecutionAgentAttempts == nil &&
		record.Outcome != localruntime.IssueChecksPending
}

func (r issueCacheReclaimer) reclaimWorkspace(ctx context.Context, root *os.Root, name, dir, path, device string, free, targetFree uint64, record localruntime.IssueClaimRecord) (uint64, error) {
	key := localruntime.IssueExecutionKey(record.Run.GitHubIssue)
	lock, err := r.claims.TryControlLock(key)
	if err != nil || lock == nil {
		return free, err
	}
	defer lock.Close()
	cache := filepath.Join(name, workspace.AgentIODirName, "cache")
	caches := []string{filepath.Join(cache, "npm", "_cacache"), filepath.Join(cache, "go-build")}
	// Avoid rereading the journal for workspaces whose caches are already gone.
	caches = slices.DeleteFunc(caches, func(rel string) bool {
		return errors.Is(checkIssueCachePath(root, rel), os.ErrNotExist)
	})
	if len(caches) == 0 {
		return free, nil
	}

	// A retry can inherit another attempt's directory. Recheck every reference
	// after taking the same lock used by execution and recovery.
	records, err := r.claims.List()
	if err != nil {
		return free, err
	}
	found := false
	for _, current := range records {
		currentDir, err := filepath.Abs(current.WorkspaceDir)
		if err != nil || currentDir != dir {
			continue
		}
		if !canReclaimIssueCaches(current) || localruntime.IssueExecutionKey(current.Run.GitHubIssue) != key {
			return free, nil
		}
		stopped, err := localruntime.IssueProcessesStopped(current)
		if err != nil || !stopped {
			return free, err
		}
		found = found || current.ID == record.ID
	}
	if !found {
		return free, nil
	}

	var problems []error
	// npm stores diagnostic logs next to _cacache. Keep those and all other
	// tool state, including module downloads, outside the deletion allowlist.
	for _, rel := range caches {
		if ctx.Err() != nil || free >= targetFree {
			break
		}
		if err := checkIssueCachePath(root, rel); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			problems = append(problems, err)
			continue
		}
		cachePath := filepath.Join(root.Name(), rel)
		cacheDevice, _, _, err := r.diskSpace(cachePath)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if cacheDevice != device {
			continue
		}
		if err := workspace.CheckCacheMounts(root.Name(), cachePath); err != nil {
			problems = append(problems, err)
			continue
		}
		if err := r.remove(root, rel); err != nil {
			problems = append(problems, fmt.Errorf("remove cache %s: %w", rel, err))
		} else {
			r.logf("workspace_storage status=ok action=remove_issue_cache claim_id=%s cache=%q", record.ID, filepath.Base(rel))
		}
		pool, after, _, err := r.diskSpace(path)
		if err != nil || pool != device {
			if err == nil {
				err = fmt.Errorf("workspace filesystem changed during cache cleanup")
			}
			return free, errors.Join(append(problems, err)...)
		}
		free = after
	}
	return free, errors.Join(problems...)
}

func checkIssueCachePath(root *os.Root, rel string) error {
	var path string
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		path = filepath.Join(path, component)
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("cache path %s is not a directory", path)
		}
	}
	return nil
}
