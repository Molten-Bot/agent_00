package localruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/Molten-Bot/agent_00/internal/workspace"
)

// TaskDiskHistory stores the largest completed workspace for each repository
// list. The keys contain no repository URLs, prompts, or workspace paths.
type TaskDiskHistory struct {
	path  string
	mu    sync.Mutex
	max   map[string]uint64
	dirty bool
}

func OpenTaskDiskHistory(path string) (*TaskDiskHistory, error) {
	history := &TaskDiskHistory{path: path, max: make(map[string]uint64)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return history, nil
	}
	if err != nil {
		return history, err
	}

	var maxima map[string]uint64
	if err := json.Unmarshal(data, &maxima); err != nil {
		return history, fmt.Errorf("decode task disk history: %w", err)
	}
	if maxima != nil {
		history.max = maxima
	}
	return history, nil
}

// TaskDiskKey identifies the repositories checked out by a task, independent
// of checkout order. Multiplicity matters because each checkout uses space.
func TaskDiskKey(repos []string) string {
	if len(repos) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(repos))
	for _, repo := range repos {
		ref := strings.TrimSpace(repo)
		if strings.HasPrefix(strings.ToLower(ref), "git@github.com:") {
			ref = "ssh://github.com/" + ref[len("git@github.com:"):]
		}
		if parsed, err := url.Parse(ref); err == nil && strings.EqualFold(parsed.Hostname(), "github.com") {
			path := strings.ToLower(strings.Trim(parsed.Path, "/"))
			if strings.Count(path, "/") == 1 {
				ref = "https://github.com/" + strings.TrimSuffix(path, ".git")
			}
		}
		sorted = append(sorted, ref)
	}
	sort.Strings(sorted)
	data, _ := json.Marshal(sorted) // A string slice always encodes.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *TaskDiskHistory) RequiredBytes(key string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.max[key]
}

func (h *TaskDiskHistory) Record(key string, size uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if size > h.max[key] && key != "" {
		h.max[key] = size
		h.dirty = true
	}
	if !h.dirty {
		return nil
	}

	data, err := json.Marshal(h.max)
	if err == nil {
		err = writeRuntimeConfigFile(h.path, data)
	}
	if err != nil {
		return fmt.Errorf("save task disk history: %w", err)
	}
	h.dirty = false
	return nil
}

func (c *AdaptiveScheduler) reserveDiskLocked(waiter *taskWaiter, force bool) bool {
	if waiter.disk == nil {
		return true
	}
	required := uint64(workspace.MinimumFreeBytes)
	if waiter.disk.RequiredBytes != nil {
		required = waiter.disk.RequiredBytes()
	}
	var fallbackRoot, fallbackDevice string
	var fallbackAvailable uint64
	for _, root := range waiter.disk.Workspace.DiskRoots() {
		path := root
		if waiter.disk.ExistingDir != "" {
			path = waiter.disk.ExistingDir
		}
		device, free, err := c.diskSpace(path)
		if err != nil {
			continue
		}
		// Reserve each running task's full allowance. Free space may already
		// reflect some of its writes, so this is deliberately conservative.
		available := free - min(free, c.reserved[device])
		if available >= max(workspace.MinimumFreeBytes, required) {
			manager := waiter.disk.Workspace
			manager.Root = root
			if err := manager.PrepareRoots(); err != nil {
				continue
			}
			c.setDiskReservation(waiter, root, device, required)
			return true
		}
		if fallbackRoot == "" {
			fallbackRoot, fallbackDevice, fallbackAvailable = root, device, available
		}
	}
	if force && fallbackRoot != "" {
		c.setDiskReservation(waiter, fallbackRoot, fallbackDevice, min(required, fallbackAvailable))
	}
	return force
}

func (c *AdaptiveScheduler) setDiskReservation(waiter *taskWaiter, root, device string, size uint64) {
	waiter.root, waiter.device, waiter.diskBytes = root, device, size
	if size > 0 {
		c.reserved[device] += size
	}
}
