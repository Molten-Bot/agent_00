package localruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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
	if path == "" {
		return history, nil
	}
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
	return h.flushLocked()
}

// Flush retries any unsaved measurements without recording a new sample.
func (h *TaskDiskHistory) Flush() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushLocked()
}

func (h *TaskDiskHistory) flushLocked() error {
	if h.path == "" || !h.dirty {
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

type diskSample struct {
	device string
	free   uint64
	err    error
}

type diskReservation struct {
	total       big.Int
	reclaimable big.Int
}

func (c *AdaptiveScheduler) reserveDiskLocked(waiter *taskWaiter, force bool, samples map[string]diskSample, draining map[string]struct{}) (bool, error) {
	if waiter.disk == nil {
		return true, nil
	}
	if !force && time.Now().Before(waiter.disk.RetryAfter) {
		return false, nil
	}
	required := uint64(workspace.MinimumFreeBytes)
	if waiter.disk.RequiredBytes != nil {
		required = waiter.disk.RequiredBytes()
	}
	var fallbackRoot, fallbackDevice string
	var setupErr error
	blocked := false
	manager := waiter.disk.Workspace
	if !force {
		// Disk admission handles free space. PrepareRoots checks setup even
		// when capacity is too low for this task to start.
		manager.CheckSpace = nil
	}
	roots := manager.DiskRoots()
	if i := slices.Index(roots, waiter.disk.RetryRoot); i >= 0 && i < len(roots)-1 {
		failed := roots[i]
		copy(roots[i:], roots[i+1:])
		roots[len(roots)-1] = failed
	}
	waiter.drainDevices = waiter.drainDevices[:0]
	for _, root := range roots {
		manager.Root = root
		if err := manager.PrepareRoots(); err != nil {
			if !force && IsWorkspaceDiskFull(err) {
				blocked = true
			} else {
				setupErr = err
			}
			continue
		}
		path := root
		if waiter.disk.ExistingDir != "" {
			path = waiter.disk.ExistingDir
		}
		sample, ok := samples[path]
		if !ok {
			sample.device, sample.free, sample.err = c.diskSpace(path)
			samples[path] = sample
		}
		if sample.err != nil {
			setupErr = fmt.Errorf("inspect workspace storage %s: %w", path, sample.err)
			continue
		}
		// Reserve each running task's full allowance. Free space may already
		// reflect some of its writes, so this is deliberately conservative.
		device, available := sample.device, sample.free
		if _, olderWaiting := draining[device]; olderWaiting {
			blocked = true
			continue
		}
		reservation := c.reserved[device]
		if reservation != nil {
			if reservation.total.IsUint64() {
				available -= min(available, reservation.total.Uint64())
			} else {
				available = 0
			}
		}
		minimum := max(workspace.MinimumFreeBytes, required)
		fits := available >= minimum
		if !fits && !force {
			blocked = true
			// Cleanup can restore space already consumed by active workspaces.
			// Use their reserved sizes as estimates for draining only.
			if sample.free >= minimum || (reservation != nil &&
				reservation.reclaimable.Cmp(new(big.Int).SetUint64(minimum-sample.free)) >= 0) {
				waiter.drainDevices = append(waiter.drainDevices, device)
			}
			continue
		}
		if fits {
			c.setDiskReservation(waiter, root, device, required)
			return true, nil
		}
		if fallbackRoot == "" {
			fallbackRoot, fallbackDevice = root, device
		}
	}
	if fallbackRoot != "" {
		c.setDiskReservation(waiter, fallbackRoot, fallbackDevice, required)
		return true, nil
	}
	if blocked {
		return false, nil
	}
	return false, setupErr
}

func (c *AdaptiveScheduler) setDiskReservation(waiter *taskWaiter, root, device string, size uint64) {
	waiter.root, waiter.device, waiter.diskBytes = root, device, size
	if size > 0 {
		reservation := c.reserved[device]
		if reservation == nil {
			reservation = &diskReservation{}
			c.reserved[device] = reservation
		}
		bytes := new(big.Int).SetUint64(size)
		reservation.total.Add(&reservation.total, bytes)
		if waiter.disk.CleanupOnCompletion {
			reservation.reclaimable.Add(&reservation.reclaimable, bytes)
		}
	}
}
