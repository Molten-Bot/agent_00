package localruntime

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Molten-Bot/agent_00/internal/stringsx"
	"github.com/Molten-Bot/agent_00/internal/workspace"
)

var errSchedulerClosed = errors.New("task controller is closed")

// AdaptiveScheduler controls task admission with rolling system load samples.
// It never preempts running work; when pressure rises, newer tasks stay queued.
type AdaptiveScheduler struct {
	cfg       SchedulerConfig
	logf      func(string, ...any)
	sample    resourceSampler
	diskSpace func(string) (string, uint64, error)

	mu       sync.Mutex
	waiters  []*taskWaiter
	reserved map[string]*diskReservation
	running  int
	allowed  int
	window   []resourceSample
	closed   bool

	startOnce sync.Once
	stopCh    chan struct{}
	nextID    atomic.Uint64
}

type taskWaiter struct {
	id           uint64
	requestID    string
	granted      bool
	err          error
	notify       chan struct{}
	disk         *TaskDiskRequest
	root         string
	device       string
	diskBytes    uint64
	diskBlocked  bool
	drainDevices []string
}

// TaskDiskRequest describes the storage needed for a task workspace.
type TaskDiskRequest struct {
	Workspace     workspace.Manager
	ExistingDir   string
	RequiredBytes func() uint64
	// CleanupOnCompletion identifies workspaces removed before reservation release.
	CleanupOnCompletion bool
	// RetryRoot is the root whose allocation failed. Try it after other roots.
	RetryRoot string
	// RetryAfter delays normal admission after a disk-full allocation failure.
	RetryAfter time.Time
}

type resourceSample struct {
	CPUPercent    float64
	MemoryPercent float64
	DiskIOMBs     float64
}

const schedulerResumeThresholdPercent = 65.0

type resourceSampler interface {
	Sample() (resourceSample, error)
}

type defaultResourceSampler struct {
	mu           sync.Mutex
	lastCPUTotal uint64
	lastCPUIdle  uint64
	lastDiskIO   uint64
	lastDiskTS   time.Time
	haveCPU      bool
	haveDisk     bool

	diskSamplingConfigured bool
	diskSamplingEnabled    bool
	diskZeroTotalSamples   int
}

// WithSchedulerDiskSpace supplies the scheduler's free-space sampler.
func WithSchedulerDiskSpace(sample func(string) (string, uint64, error)) func(*AdaptiveScheduler) {
	return func(c *AdaptiveScheduler) { c.diskSpace = sample }
}

// NewAdaptiveScheduler returns a queue-aware, adaptive scheduler.
func NewAdaptiveScheduler(cfg SchedulerConfig, logf func(string, ...any), options ...func(*AdaptiveScheduler)) *AdaptiveScheduler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg = normalizeSchedulerConfig(cfg)
	c := &AdaptiveScheduler{
		cfg:       cfg,
		logf:      logf,
		sample:    &defaultResourceSampler{},
		diskSpace: workspace.DiskSpace,
		reserved:  make(map[string]*diskReservation),
		allowed:   cfg.MaxParallel,
		stopCh:    make(chan struct{}),
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// Start begins background sampling. Safe to call multiple times.
func (c *AdaptiveScheduler) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		interval := time.Duration(c.cfg.SampleIntervalMS) * time.Millisecond
		if interval < 250*time.Millisecond {
			interval = 250 * time.Millisecond
		}

		go func() {
			c.sampleAndUpdate()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					c.close()
					return
				case <-ticker.C:
					c.sampleAndUpdate()
				}
			}
		}()
	})
}

// Acquire blocks until the caller is admitted to run. The returned function must be called once when done.
func (c *AdaptiveScheduler) Acquire(ctx context.Context, requestID string) (func(), error) {
	_, release, err := c.acquire(ctx, requestID, nil)
	return release, err
}

// AcquireWorkspace admits work and returns the workspace root whose space was reserved.
func (c *AdaptiveScheduler) AcquireWorkspace(ctx context.Context, requestID string, disk TaskDiskRequest, force bool) (string, func(), error) {
	if force {
		return c.acquireForce(ctx, requestID, &disk)
	}
	return c.acquire(ctx, requestID, &disk)
}

func (c *AdaptiveScheduler) acquire(ctx context.Context, requestID string, disk *TaskDiskRequest) (string, func(), error) {
	if c == nil {
		return "", func() {}, nil
	}

	requestID = strings.TrimSpace(requestID)
	waiter := &taskWaiter{
		id:        c.nextID.Add(1),
		requestID: requestID,
		notify:    make(chan struct{}, 1),
		disk:      disk,
	}

	var (
		queued      bool
		granted     bool
		queueDepth  int
		running     int
		allowed     int
		queueReason string
		acquireErr  error
	)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", nil, errSchedulerClosed
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return "", nil, err
	}
	c.waiters = append(c.waiters, waiter)
	// Earlier waiters have already been considered whenever a slot is free.
	// Retry disk-blocked waiters on a scheduler tick or when work releases space.
	c.tryGrantLocked(len(c.waiters) - 1)
	granted = waiter.granted
	acquireErr = waiter.err
	queued = !granted && acquireErr == nil
	if queued {
		queueDepth = len(c.waiters)
		running = c.running
		allowed = c.allowed
		if waiter.diskBlocked {
			queueReason = " reason=disk_space"
		}
	}
	c.mu.Unlock()

	if queued {
		c.logf(
			"task status=queued request_id=%s queue_depth=%d running=%d allowed=%d%s",
			stringsx.FirstNonEmpty(waiter.requestID, fmt.Sprintf("queued-%d", waiter.id)),
			queueDepth,
			running,
			allowed,
			queueReason,
		)
		// Re-sample immediately after queueing so work does not sit idle until the
		// next background tick when pressure has already dropped back below the
		// resume threshold.
		if queueReason == "" {
			c.sampleResources(false)
		}
	}

	if acquireErr != nil {
		return "", nil, acquireErr
	}
	if granted {
		return waiter.root, c.releaseFunc(waiter), nil
	}

	for {
		select {
		case <-waiter.notify:
			c.mu.Lock()
			granted, err := waiter.granted, waiter.err
			c.mu.Unlock()
			if err != nil {
				return "", nil, err
			}
			if granted {
				return waiter.root, c.releaseFunc(waiter), nil
			}
		case <-ctx.Done():
			c.mu.Lock()
			if waiter.granted {
				c.releaseLocked(waiter)
				c.tryGrantLocked(0)
			} else {
				c.removeWaiterLocked(waiter.id)
				if len(waiter.drainDevices) > 0 {
					c.tryGrantLocked(0)
				}
			}
			c.mu.Unlock()
			return "", nil, ctx.Err()
		case <-c.stopCh:
			c.mu.Lock()
			if waiter.granted {
				c.releaseLocked(waiter)
			} else {
				c.removeWaiterLocked(waiter.id)
			}
			c.mu.Unlock()
			return "", nil, errSchedulerClosed
		}
	}
}

// AcquireForce admits a caller immediately, even when current running work exceeds allowed capacity.
// Use this sparingly for explicit operator override actions.
func (c *AdaptiveScheduler) AcquireForce(ctx context.Context, requestID string) (func(), error) {
	_, release, err := c.acquireForce(ctx, requestID, nil)
	return release, err
}

func (c *AdaptiveScheduler) acquireForce(ctx context.Context, requestID string, disk *TaskDiskRequest) (string, func(), error) {
	if c == nil {
		return "", func() {}, nil
	}

	select {
	case <-ctx.Done():
		return "", nil, ctx.Err()
	default:
	}

	requestID = strings.TrimSpace(requestID)
	waiter := &taskWaiter{disk: disk}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", nil, errSchedulerClosed
	}
	if _, err := c.reserveDiskLocked(waiter, true, make(map[string]diskSample), nil); err != nil {
		c.mu.Unlock()
		return "", nil, err
	}
	c.running++
	running := c.running
	allowed := c.allowed
	c.mu.Unlock()

	c.logf(
		"task status=forced request_id=%s running=%d allowed=%d",
		stringsx.FirstNonEmpty(requestID, "forced"),
		running,
		allowed,
	)

	return waiter.root, c.releaseFunc(waiter), nil
}

func (c *AdaptiveScheduler) releaseFunc(waiter *taskWaiter) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.releaseLocked(waiter)
			c.tryGrantLocked(0)
			c.mu.Unlock()
		})
	}
}

func (c *AdaptiveScheduler) releaseLocked(waiter *taskWaiter) {
	c.running--
	if waiter.diskBytes > 0 {
		reservation := c.reserved[waiter.device]
		bytes := new(big.Int).SetUint64(waiter.diskBytes)
		reservation.total.Sub(&reservation.total, bytes)
		if waiter.disk.CleanupOnCompletion {
			reservation.reclaimable.Sub(&reservation.reclaimable, bytes)
		}
		if reservation.total.Sign() == 0 {
			delete(c.reserved, waiter.device)
		}
	}
}

func (c *AdaptiveScheduler) tryGrantLocked(start int) {
	if c.running >= c.allowed || start >= len(c.waiters) {
		return
	}
	samples := make(map[string]diskSample)
	draining := make(map[string]struct{})
	for _, older := range c.waiters[:start] {
		for _, device := range older.drainDevices {
			draining[device] = struct{}{}
		}
	}
	for i := start; c.running < c.allowed && i < len(c.waiters); {
		waiter := c.waiters[i]
		granted, err := c.reserveDiskLocked(waiter, false, samples, draining)
		if !granted && err == nil {
			waiter.diskBlocked = true
			for _, device := range waiter.drainDevices {
				draining[device] = struct{}{}
			}
			i++
			continue
		}
		c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
		waiter.granted, waiter.err = granted, err
		if granted {
			c.running++
		}
		select {
		case waiter.notify <- struct{}{}:
		default:
		}
	}
}

func (c *AdaptiveScheduler) removeWaiterLocked(id uint64) {
	for i, waiter := range c.waiters {
		if waiter.id != id {
			continue
		}
		c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
		return
	}
}

func (c *AdaptiveScheduler) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	waiters := append([]*taskWaiter(nil), c.waiters...)
	c.waiters = nil
	c.mu.Unlock()

	for _, waiter := range waiters {
		select {
		case waiter.notify <- struct{}{}:
		default:
		}
	}
	close(c.stopCh)
}

func (c *AdaptiveScheduler) sampleAndUpdate() {
	c.sampleResources(true)
}

func (c *AdaptiveScheduler) sampleResources(retryDisk bool) {
	if c == nil || c.sample == nil {
		return
	}

	sample, err := c.sample.Sample()
	if err != nil {
		c.logf("scheduler status=sample_error err=%q", err)
		if retryDisk {
			c.mu.Lock()
			c.tryGrantLocked(0)
			c.mu.Unlock()
		}
		return
	}

	c.mu.Lock()
	windowSize := c.cfg.SampleWindow
	if windowSize <= 0 {
		windowSize = 1
	}
	if len(c.window) < windowSize {
		c.window = append(c.window, sample)
	} else {
		copy(c.window, c.window[1:])
		c.window[len(c.window)-1] = sample
	}
	avg := averageResourceSample(c.window)

	prevAllowed := c.allowed
	nextAllowed := computeAllowedParallel(c.cfg, avg, prevAllowed)
	if nextAllowed < c.cfg.MinParallel {
		nextAllowed = c.cfg.MinParallel
	}
	if nextAllowed > c.cfg.MaxParallel {
		nextAllowed = c.cfg.MaxParallel
	}
	c.allowed = nextAllowed
	if retryDisk || nextAllowed > prevAllowed {
		c.tryGrantLocked(0)
	}
	queueDepth := len(c.waiters)
	running := c.running
	c.mu.Unlock()

	state := "steady"
	if prevAllowed != nextAllowed {
		state = "adjusted"
	}
	c.logf(
		"debug scheduler status=window state=%s cpu=%.1f memory=%.1f disk_io_mb_s=%.1f allowed=%d max=%d running=%d queue_depth=%d",
		state,
		avg.CPUPercent,
		avg.MemoryPercent,
		avg.DiskIOMBs,
		nextAllowed,
		c.cfg.MaxParallel,
		running,
		queueDepth,
	)
}

func averageResourceSample(values []resourceSample) resourceSample {
	if len(values) == 0 {
		return resourceSample{}
	}

	var (
		cpuSum, memSum, diskSum       float64
		cpuCount, memCount, diskCount int
	)
	for _, value := range values {
		if value.CPUPercent > 0 {
			cpuSum += value.CPUPercent
			cpuCount++
		}
		if value.MemoryPercent > 0 {
			memSum += value.MemoryPercent
			memCount++
		}
		if value.DiskIOMBs > 0 {
			diskSum += value.DiskIOMBs
			diskCount++
		}
	}

	var out resourceSample
	if cpuCount > 0 {
		out.CPUPercent = cpuSum / float64(cpuCount)
	}
	if memCount > 0 {
		out.MemoryPercent = memSum / float64(memCount)
	}
	if diskCount > 0 {
		out.DiskIOMBs = diskSum / float64(diskCount)
	}
	return out
}

func computeAllowedParallel(cfg SchedulerConfig, avg resourceSample, prevAllowed int) int {
	maxParallel := cfg.MaxParallel
	if maxParallel < 1 {
		maxParallel = 1
	}
	minParallel := cfg.MinParallel
	if minParallel < 1 {
		minParallel = 1
	}
	if prevAllowed < minParallel {
		prevAllowed = minParallel
	}
	if prevAllowed > maxParallel {
		prevAllowed = maxParallel
	}

	// Disk IO remains visible in the sample window logs, but it does not
	// participate in concurrency admission because containerized environments
	// frequently cannot report it reliably enough to gate work.
	resourceStates := []struct {
		value float64
		high  float64
	}{
		{value: avg.CPUPercent, high: cfg.CPUHighWatermark},
		{value: avg.MemoryPercent, high: cfg.MemoryHighWatermark},
	}
	availableMetrics := 0
	allBelowResumeThreshold := true
	for _, state := range resourceStates {
		if state.value <= 0 {
			continue
		}
		availableMetrics++
		if state.high > 0 && state.value > state.high {
			return minParallel
		}
		if state.value >= schedulerResumeThresholdPercent {
			allBelowResumeThreshold = false
		}
	}
	if availableMetrics == 0 {
		return maxParallel
	}
	if allBelowResumeThreshold {
		return maxParallel
	}
	return prevAllowed
}

func maxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func normalizeSchedulerConfig(cfg SchedulerConfig) SchedulerConfig {
	normalized := InitConfig{
		Scheduler: cfg,
	}
	normalized.ApplyDefaults()
	return normalized.Scheduler
}

func (s *defaultResourceSampler) Sample() (resourceSample, error) {
	// Acquire and the sampling loop can run concurrently. Keep counter reads
	// and baseline updates in one sample so deltas use a consistent snapshot.
	s.mu.Lock()
	defer s.mu.Unlock()
	switch runtime.GOOS {
	case "windows":
		return s.sampleWindows()
	case "linux":
		return s.sampleLinux()
	default:
		return resourceSample{}, nil
	}
}

func (s *defaultResourceSampler) sampleLinux() (resourceSample, error) {
	cpuTotal, cpuIdle, err := readLinuxCPUStat("/proc/stat")
	if err != nil {
		return resourceSample{}, err
	}
	memPercent, err := readLinuxMemoryUsagePercent("/proc/meminfo")
	if err != nil {
		return resourceSample{}, err
	}
	if !s.diskSamplingConfigured {
		s.diskSamplingEnabled = !shouldDisableLinuxDiskSampling(
			os.Getenv("container"),
			linuxContainerMarkerFileExists(),
			readLinuxCGroupSnapshot("/proc/1/cgroup"),
		)
		s.diskSamplingConfigured = true
	}

	sample := resourceSample{
		MemoryPercent: memPercent,
	}

	if s.haveCPU {
		totalDelta := cpuTotal - s.lastCPUTotal
		idleDelta := cpuIdle - s.lastCPUIdle
		if totalDelta > 0 {
			busy := float64(totalDelta-idleDelta) / float64(totalDelta)
			sample.CPUPercent = busy * 100
		}
	}
	s.lastCPUTotal = cpuTotal
	s.lastCPUIdle = cpuIdle
	s.haveCPU = true

	if s.diskSamplingEnabled {
		diskTotalBytes, err := readLinuxDiskIOBytes("/proc/diskstats")
		if err != nil {
			return resourceSample{}, err
		}
		if shouldDisableLinuxDiskSamplingForZeroTotals(diskTotalBytes, s.diskZeroTotalSamples) {
			s.diskSamplingEnabled = false
			s.haveDisk = false
			return sample, nil
		}
		if diskTotalBytes == 0 {
			s.diskZeroTotalSamples++
		} else {
			s.diskZeroTotalSamples = 0
		}
		now := time.Now()
		if s.haveDisk {
			if elapsed := now.Sub(s.lastDiskTS).Seconds(); elapsed > 0 {
				bytesDelta := diskTotalBytes - s.lastDiskIO
				sample.DiskIOMBs = float64(bytesDelta) / elapsed / (1024 * 1024)
			}
		}
		s.lastDiskIO = diskTotalBytes
		s.lastDiskTS = now
		s.haveDisk = true
	} else {
		s.haveDisk = false
	}

	return sample, nil
}

func (s *defaultResourceSampler) sampleWindows() (resourceSample, error) {
	// Sample three counters in one call to keep a consistent timestamp.
	cmd := exec.Command(
		"typeperf",
		"-sc", "1",
		`\\Processor(_Total)\\% Processor Time`,
		`\\Memory\\% Committed Bytes In Use`,
		`\\PhysicalDisk(_Total)\\Disk Bytes/sec`,
	)
	out, err := cmd.Output()
	if err != nil {
		return resourceSample{}, fmt.Errorf("sample windows resources: %w", err)
	}

	rows, err := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	if err != nil {
		return resourceSample{}, fmt.Errorf("parse windows resource sample: %w", err)
	}
	if len(rows) < 2 {
		return resourceSample{}, fmt.Errorf("parse windows resource sample: missing data row")
	}

	data := rows[len(rows)-1]
	if len(data) < 4 {
		return resourceSample{}, fmt.Errorf("parse windows resource sample: expected 4 columns, got %d", len(data))
	}

	cpuPercent, err := parseFlexibleFloat(data[1])
	if err != nil {
		return resourceSample{}, fmt.Errorf("parse cpu sample: %w", err)
	}
	memPercent, err := parseFlexibleFloat(data[2])
	if err != nil {
		return resourceSample{}, fmt.Errorf("parse memory sample: %w", err)
	}
	diskBytesPerSec, err := parseFlexibleFloat(data[3])
	if err != nil {
		return resourceSample{}, fmt.Errorf("parse disk sample: %w", err)
	}

	return resourceSample{
		CPUPercent:    clampFloat(cpuPercent, 0, 100),
		MemoryPercent: clampFloat(memPercent, 0, 100),
		DiskIOMBs:     maxFloat(0, diskBytesPerSec/(1024*1024)),
	}, nil
}

func parseFlexibleFloat(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"")
	if raw == "" {
		return 0, fmt.Errorf("empty float value")
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err == nil {
		return parsed, nil
	}
	if strings.Contains(raw, ",") && !strings.Contains(raw, ".") {
		raw = strings.ReplaceAll(raw, ",", ".")
		return strconv.ParseFloat(raw, 64)
	}
	return 0, err
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func readLinuxCPUStat(path string) (uint64, uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, fmt.Errorf("parse %s: malformed cpu line", path)
		}
		var (
			total uint64
			idle  uint64
		)
		for i := 1; i < len(fields); i++ {
			v, parseErr := strconv.ParseUint(fields[i], 10, 64)
			if parseErr != nil {
				return 0, 0, fmt.Errorf("parse %s cpu field %q: %w", path, fields[i], parseErr)
			}
			total += v
			if i == 4 || i == 5 {
				idle += v
			}
		}
		return total, idle, nil
	}
	return 0, 0, fmt.Errorf("parse %s: missing cpu aggregate line", path)
}

func readLinuxMemoryUsagePercent(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	var (
		totalKB uint64
		availKB uint64
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB, _ = strconv.ParseUint(fields[1], 10, 64)
		case "MemAvailable":
			availKB, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	if totalKB == 0 {
		return 0, fmt.Errorf("parse %s: missing MemTotal", path)
	}
	if availKB > totalKB {
		availKB = totalKB
	}
	usedRatio := float64(totalKB-availKB) / float64(totalKB)
	return usedRatio * 100, nil
}

func readLinuxDiskIOBytes(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	var totalSectors uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if !includeLinuxDiskDevice(name) {
			continue
		}

		readSectors, readErr := strconv.ParseUint(fields[5], 10, 64)
		writeSectors, writeErr := strconv.ParseUint(fields[9], 10, 64)
		if readErr != nil || writeErr != nil {
			continue
		}
		totalSectors += readSectors + writeSectors
	}

	return totalSectors * 512, nil
}

var (
	nvmeWholeDiskRE = regexp.MustCompile(`^nvme\d+n\d+$`)
	mmcWholeDiskRE  = regexp.MustCompile(`^mmcblk\d+$`)
	sdWholeDiskRE   = regexp.MustCompile(`^(sd|hd|vd|xvd)[a-z]+$`)
)

func includeLinuxDiskDevice(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "fd") || strings.HasPrefix(name, "sr") {
		return false
	}
	if strings.HasPrefix(name, "dm-") {
		return true
	}
	if sdWholeDiskRE.MatchString(name) {
		return true
	}
	if nvmeWholeDiskRE.MatchString(name) {
		return true
	}
	if mmcWholeDiskRE.MatchString(name) {
		return true
	}
	return false
}

func shouldDisableLinuxDiskSampling(containerEnv string, markerFileExists bool, cgroupSnapshot string) bool {
	containerEnv = strings.ToLower(strings.TrimSpace(containerEnv))
	if containerEnv != "" && containerEnv != "0" && containerEnv != "false" {
		return true
	}
	if markerFileExists {
		return true
	}
	return containsContainerCGroupMarker(cgroupSnapshot)
}

func shouldDisableLinuxDiskSamplingForZeroTotals(diskTotalBytes uint64, zeroTotalSamples int) bool {
	if diskTotalBytes > 0 {
		return false
	}
	return zeroTotalSamples >= 1
}

func linuxContainerMarkerFileExists() bool {
	for _, path := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func readLinuxCGroupSnapshot(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func containsContainerCGroupMarker(snapshot string) bool {
	snapshot = strings.ToLower(snapshot)
	if strings.TrimSpace(snapshot) == "" {
		return false
	}
	for _, marker := range []string{
		"docker",
		"containerd",
		"kubepods",
		"podman",
		"libpod",
		"lxc",
	} {
		if strings.Contains(snapshot, marker) {
			return true
		}
	}
	return false
}
