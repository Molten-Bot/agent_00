package localruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	taskDiskHistoryWindow = 4
	taskDiskSampleMaxAge  = 7 * 24 * time.Hour
	taskDiskLegacyMaxAge  = 24 * time.Hour
)

// TaskDiskHistory stores recent successful workspace sizes for each repository
// list. The keys contain no repository URLs, prompts, or workspace paths.
type TaskDiskHistory struct {
	path      string
	now       func() time.Time
	mu        sync.Mutex
	samples   map[string]taskDiskSamples
	fallback  map[string]taskDiskSamples
	dirty     bool
	needsLoad bool
}

type taskDiskSample struct {
	Bytes     uint64    `json:"bytes"`
	ExpiresAt time.Time `json:"expiresAt"`
	Temporary bool      `json:"temporary,omitempty"`

	scalarFallback bool // Used only in memory while the versioned file is unreadable.
}

func (s taskDiskSample) peakActive(now time.Time) bool {
	return s.scalarFallback || now.Before(s.ExpiresAt)
}

type taskDiskSamples []taskDiskSample

func (s taskDiskSamples) estimate(now time.Time) uint64 {
	var peak, largest, baseline uint64
	for _, sample := range s {
		if sample.peakActive(now) {
			peak = max(peak, sample.Bytes)
		}
		// Keep the largest size supported by at least two successful runs.
		// Imported snapshots only contribute to the temporary peak.
		if sample.Temporary {
			continue
		}
		if sample.Bytes >= largest {
			largest, baseline = sample.Bytes, largest
		} else {
			baseline = max(baseline, sample.Bytes)
		}
	}
	return max(peak, baseline)
}

func recentTaskDiskSamples(samples taskDiskSamples, now time.Time) taskDiskSamples {
	samples = slices.DeleteFunc(samples, func(sample taskDiskSample) bool {
		return sample.Temporary && !sample.peakActive(now)
	})
	return samples[max(0, len(samples)-taskDiskHistoryWindow):]
}

// TaskDiskSamplesPath returns the versioned sidecar for timestamped samples.
// The original path retains the scalar format used by older releases.
func TaskDiskSamplesPath(path string) string {
	return path + ".v2"
}

func OpenTaskDiskHistory(path string) (*TaskDiskHistory, error) {
	history := &TaskDiskHistory{path: path, now: time.Now, samples: make(map[string]taskDiskSamples)}
	if path == "" {
		return history, nil
	}
	samples, err := readTaskDiskSamples(path)
	history.needsLoad = err != nil
	if samples != nil {
		now := history.now()
		for key, entries := range samples {
			samples[key] = recentTaskDiskSamples(entries, now)
		}
		if err != nil {
			// Use readable history for estimates while keeping pending writes
			// separate so a successful reload won't count fallback data twice.
			history.fallback = samples
		} else {
			history.samples = samples
		}
	}
	return history, err
}

func readTaskDiskSamples(path string) (map[string]taskDiskSamples, error) {
	samples := make(map[string]taskDiskSamples)
	versionedAt, versionedErr := readTaskDiskHistoryFile(TaskDiskSamplesPath(path), &samples)
	if versionedErr != nil {
		samples = nil
	}
	var legacy map[string]json.RawMessage
	legacyAt, err := readTaskDiskHistoryFile(path, &legacy)
	if err != nil {
		return samples, errors.Join(versionedErr, err)
	}
	if !legacyAt.After(versionedAt) {
		return samples, versionedErr
	}
	imports := make(map[string]taskDiskSamples)
	for key, value := range legacy {
		var sizes []uint64
		var scalarFallback bool
		if value[0] == '[' {
			err = json.Unmarshal(value, &sizes)
		} else {
			var size uint64
			err = json.Unmarshal(value, &size)
			scalarFallback = versionedErr != nil
			// A newer scalar snapshot can contain a rollback estimate or
			// a measurement whose versioned write did not complete.
			// A versioned peak may have expired before the scalar write.
			if size > samples[key].estimate(legacyAt) {
				sizes = []uint64{size}
			}
		}
		if err != nil {
			return samples, errors.Join(versionedErr, fmt.Errorf("decode legacy task disk history: %w", err))
		}
		for _, size := range sizes {
			imports[key] = append(imports[key], taskDiskSample{
				Bytes: size, ExpiresAt: legacyAt.Add(taskDiskLegacyMaxAge), Temporary: true,
				scalarFallback: scalarFallback,
			})
		}
	}
	if samples == nil {
		samples = make(map[string]taskDiskSamples)
	}
	for key, entries := range imports {
		samples[key] = append(samples[key], entries...)
	}
	return samples, versionedErr
}

func readTaskDiskHistoryFile(path string, dest any) (time.Time, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return time.Time{}, err
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return time.Time{}, fmt.Errorf("decode task disk history %s: %w", path, err)
	}
	return info.ModTime(), nil
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
	now := h.now()
	return max(h.samples[key].estimate(now), h.fallback[key].estimate(now))
}

// Record adds a measurement from a successful task, including its tool caches.
func (h *TaskDiskHistory) Record(key string, size uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if key != "" && size > 0 {
		now := h.now()
		sample := taskDiskSample{Bytes: size, ExpiresAt: now.Add(taskDiskSampleMaxAge).UTC()}
		h.samples[key] = recentTaskDiskSamples(append(h.samples[key], sample), now)
		h.dirty = true
	}
	return h.flushLocked()
}

// Flush retries loading history and saving pending measurements.
func (h *TaskDiskHistory) Flush() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushLocked()
}

func (h *TaskDiskHistory) flushLocked() error {
	if h.path == "" || (!h.needsLoad && !h.dirty) {
		return nil
	}
	if h.needsLoad {
		// Keep the pending measurements newer than any recovered history.
		samples, err := readTaskDiskSamples(h.path)
		if err != nil {
			return fmt.Errorf("reload task disk history before saving: %w", err)
		}
		if samples == nil {
			samples = make(map[string]taskDiskSamples)
		}
		for key, pending := range h.samples {
			samples[key] = append(samples[key], pending...)
		}
		h.samples = samples
		h.fallback = nil
		h.needsLoad = false
	}

	now := h.now()
	estimates := make(map[string]uint64)
	for key, samples := range h.samples {
		samples = recentTaskDiskSamples(samples, now)
		if len(samples) == 0 {
			delete(h.samples, key)
			continue
		}
		// Expired successful samples can still establish a baseline with a later run.
		h.samples[key] = samples
		if estimate := samples.estimate(now); estimate > 0 {
			estimates[key] = estimate
		}
	}
	if !h.dirty {
		return nil
	}
	// Write the rollback-compatible snapshot first so a newer scalar file
	// can only come from an older release or an interrupted save.
	if err := writeTaskDiskHistoryFile(h.path, estimates); err != nil {
		return fmt.Errorf("save task disk history: %w", err)
	}
	if err := writeTaskDiskHistoryFile(TaskDiskSamplesPath(h.path), h.samples); err != nil {
		return fmt.Errorf("save task disk history: %w", err)
	}
	h.dirty = false
	return nil
}

func writeTaskDiskHistoryFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRuntimeConfigFile(path, data)
}

type diskSample struct {
	device string
	free   uint64
	total  uint64
	err    error
}

type diskReservation struct {
	total             big.Int
	cleanupWorkspaces int
	holders           map[*taskWaiter]struct{}
}

type diskReclaim struct {
	path, device string
	targetFree   uint64
}

func (c *AdaptiveScheduler) reclaimDiskLocked(request diskReclaim) {
	// Zero progress (including a cleanup error) should not rescan the journal
	// on every scheduler tick. The zero deadline marks an in-flight cleanup.
	if deadline, ok := c.reclaims[request.device]; ok && (deadline.IsZero() || time.Now().Before(deadline)) {
		return
	}
	c.reclaims[request.device] = time.Time{}
	go func() {
		c.reclaimDisk(request.path, request.device, request.targetFree)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.reclaims[request.device] = time.Now().Add(30 * time.Second)
		c.tryGrantLocked(0)
	}()
}

func (w *taskWaiter) explainDiskBlock(path string, sample diskSample, minimum uint64, reservation *diskReservation) {
	if w.diskBlock != "" {
		return
	}
	var reserved string
	var holders []string
	if reservation != nil {
		reserved = reservation.total.String()
		for holder := range reservation.holders {
			if holder.diskBytes > 0 && holder.requestID != "" {
				holders = append(holders, holder.requestID)
			}
		}
		sort.Strings(holders)
	} else {
		reserved = "0"
	}
	w.diskBlock = fmt.Sprintf("path=%q device=%q free_bytes=%d required_bytes=%d reserved_bytes=%s blocking_request_ids=%q estimated_bytes=%d minimum_free_bytes=%d",
		path, sample.device, sample.free, minimum, reserved, strings.Join(holders, ","), w.diskEstimate, workspace.MinimumFreeBytes)
}

// IsWorkspaceDiskFull reports whether workspace allocation ran out of space.
func IsWorkspaceDiskFull(err error) bool {
	return workspace.IsDiskFull(err)
}

func (c *AdaptiveScheduler) reserveDiskLocked(waiter *taskWaiter, force bool, samples map[string]diskSample, preparedRoots map[string]error) (bool, error) {
	if waiter.disk == nil {
		return true, nil
	}
	waiter.drainDevices = waiter.drainDevices[:0]
	waiter.diskBlock = ""
	required := uint64(workspace.MinimumFreeBytes)
	if waiter.disk.RequiredBytes != nil {
		required = waiter.disk.RequiredBytes()
	}
	waiter.diskEstimate = required
	waiter.diskCooldown = !force && time.Now().Before(waiter.disk.RetryAfter)
	if waiter.diskCooldown {
		waiter.diskBlock = fmt.Sprintf("retry_at=%s estimated_bytes=%d", waiter.disk.RetryAfter.UTC().Format(time.RFC3339Nano), required)
		return false, nil
	}
	minimum := max(workspace.MinimumFreeBytes, required)
	var fallbackRoot, fallbackDevice string
	var setupErr error
	var blocked, tooSmall bool
	var reclaims []diskReclaim
	manager := waiter.disk.Workspace
	checkPersistent := waiter.disk.CheckPersistentStorage
	if !force || checkPersistent != nil {
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
	for _, root := range roots {
		if root == waiter.disk.PermissionDeniedRoot {
			if setupErr == nil {
				setupErr = fmt.Errorf("allocate workspace under %s: %w", root, os.ErrPermission)
			}
			continue
		}
		err, prepared := preparedRoots[root]
		if !prepared {
			manager.Root = root
			err = manager.PrepareRoots()
			if !force {
				preparedRoots[root] = err
			}
		}
		if err != nil {
			if !force && IsWorkspaceDiskFull(err) {
				blocked = true
				if waiter.diskBlock == "" {
					waiter.diskBlock = fmt.Sprintf("path=%q err=%q estimated_bytes=%d", root, err, required)
				}
			} else {
				setupErr = err
			}
			continue
		}
		path := root
		if waiter.disk.ExistingDir != "" {
			path = waiter.disk.ExistingDir
		}
		if checkPersistent != nil {
			if err := checkPersistent(path); err != nil {
				setupErr = err
				continue
			}
		}
		sample, ok := samples[path]
		if !ok {
			sample.device, sample.free, sample.total, sample.err = c.diskSpace(path)
			samples[path] = sample
		}
		if sample.err != nil {
			setupErr = fmt.Errorf("inspect workspace storage %s: %w", path, sample.err)
			continue
		}
		if checkPersistent != nil && sample.total < workspace.MinimumFreeBytes {
			// Only the fixed floor makes this volume unsuitable. An estimate
			// can shrink while the task waits for admission.
			setupErr = fmt.Errorf("%w: %s has capacity %d bytes and %d bytes free, requires %d bytes; expand or move the workspace filesystem",
				workspace.ErrUnsuitableStorage, path, sample.total, sample.free, workspace.MinimumFreeBytes)
			continue
		}
		if force && checkPersistent != nil && sample.free < workspace.MinimumFreeBytes {
			setupErr = fmt.Errorf("%w: %s has %d bytes free, requires %d bytes; free up workspace storage",
				workspace.ErrDiskFull, path, sample.free, workspace.MinimumFreeBytes)
			continue
		}
		if !force && minimum > sample.total {
			// Draining cannot make this estimate fit. Keep Force start available
			// unless the allocator itself rejects this undersized root.
			checkSpace := waiter.disk.Workspace.CheckSpace
			if sample.total < workspace.MinimumFreeBytes && checkSpace != nil && checkSpace(root) != nil {
				tooSmall = true
			} else {
				blocked = true
				waiter.explainDiskBlock(path, sample, minimum, c.reserved[sample.device])
			}
			continue
		}
		// Reserve each running task's full allowance. Free space may already
		// reflect some of its writes, so this is deliberately conservative.
		device, available := sample.device, sample.free
		reservation := c.reserved[device]
		if draining := c.draining[device]; !force && draining != nil {
			blocked = true
			if waiter.diskBlock == "" {
				waiter.explainDiskBlock(path, sample, minimum, reservation)
				waiter.diskBlock += fmt.Sprintf(" draining_for=%q", draining.requestID)
			}
			continue
		}
		var reserved uint64
		if reservation != nil {
			if reservation.total.IsUint64() {
				reserved = reservation.total.Uint64()
			} else {
				reserved = ^uint64(0)
			}
			available -= min(available, reserved)
		}
		fits := available >= minimum
		if !fits && !force {
			blocked = true
			waiter.explainDiskBlock(path, sample, minimum, reservation)
			if c.reclaimDisk != nil && reserved <= sample.total-minimum {
				reclaims = append(reclaims, diskReclaim{path, device, minimum + reserved})
			}
			// Active workspaces may have written past their reservations.
			// Drain until cleanup reveals how much space the pool can recover.
			if sample.free >= minimum || (reservation != nil && reservation.cleanupWorkspaces > 0) {
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
		for _, request := range reclaims {
			c.reclaimDiskLocked(request)
		}
		return false, nil
	}
	if setupErr == nil && tooSmall {
		return false, fmt.Errorf("workspace storage requires %d bytes, exceeding total filesystem capacity", workspace.MinimumFreeBytes)
	}
	return false, setupErr
}

func (c *AdaptiveScheduler) setDiskReservation(waiter *taskWaiter, root, device string, size uint64) {
	waiter.root, waiter.device, waiter.diskBytes = root, device, size
	if size > 0 || waiter.disk.CleanupOnCompletion {
		reservation := c.reserved[device]
		if reservation == nil {
			reservation = &diskReservation{holders: make(map[*taskWaiter]struct{})}
			c.reserved[device] = reservation
		}
		bytes := new(big.Int).SetUint64(size)
		reservation.total.Add(&reservation.total, bytes)
		reservation.holders[waiter] = struct{}{}
		if waiter.disk.CleanupOnCompletion {
			reservation.cleanupWorkspaces++
		}
	}
}
