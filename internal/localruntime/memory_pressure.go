package localruntime

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const kibPerMiB = 1024

type linuxMemory struct {
	total, available, swapTotal, swapFree, shared uint64 // KiB from /proc/meminfo
}

func readLinuxMemory(path string) (linuxMemory, error) {
	var memory linuxMemory
	data, err := os.ReadFile(path)
	if err != nil {
		return memory, fmt.Errorf("read %s: %w", path, err)
	}
	fields := map[string]*uint64{
		"MemTotal": &memory.total, "MemAvailable": &memory.available,
		"SwapTotal": &memory.swapTotal, "SwapFree": &memory.swapFree, "Shmem": &memory.shared,
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(line, ":")
		dest, ok := fields[key]
		if !ok {
			continue
		}
		parts := strings.Fields(value)
		if len(parts) == 0 {
			continue
		}
		if kb, err := strconv.ParseUint(parts[0], 10, 64); err == nil {
			*dest = kb
			delete(fields, key)
		}
	}
	if memory.total == 0 || fields["MemAvailable"] != nil {
		return memory, fmt.Errorf("parse %s: missing or invalid MemTotal or MemAvailable", path)
	}
	return memory, nil
}

type memoryPressure struct {
	mu       sync.Mutex
	nextRead time.Time
	paused   bool
	read     func() (linuxMemory, error)
	now      func() time.Time
	logf     func(string, ...any)
}

// NewMemoryPressureCheck shares a cheap host-memory check between admission and
// background polling. Running tasks keep their slots and can finish normally.
func NewMemoryPressureCheck(logf func(string, ...any)) func() bool {
	if runtime.GOOS != "linux" {
		return nil
	}
	check := &memoryPressure{
		read: func() (linuxMemory, error) { return readLinuxMemory("/proc/meminfo") },
		now:  time.Now,
		logf: logf,
	}
	return check.check
}

func (p *memoryPressure) check() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if now.Before(p.nextRead) {
		return p.paused
	}
	p.nextRead = now.Add(time.Second)
	memory, err := p.read()
	if err != nil {
		return p.paused
	}
	// Use a separate resume threshold so reclaim near the limit does not
	// repeatedly start and pause work. Zero configured swap counts as no headroom.
	paused := memory.available <= min(128*kibPerMiB, memory.total/8) ||
		(memory.available <= min(512*kibPerMiB, memory.total/4) && memory.swapFree <= 128*kibPerMiB)
	if p.paused {
		paused = memory.available < min(768*kibPerMiB, memory.total/3)
	}
	if paused == p.paused {
		return paused
	}
	p.paused = paused
	if p.logf != nil {
		status, state, action := "warn", "paused", "check Go/Chromium child RSS and tmpfs usage (/tmp, /dev/shm); use disk-backed task storage and preinstalled tools"
		if !paused {
			status, state, action = "ok", "resumed", "resume background GitHub polling and queued tasks"
		}
		p.logf("host_memory_pressure status=%s state=%s mem_available_mib=%d mem_total_mib=%d swap_free_mib=%d swap_total_mib=%d shmem_mib=%d action=%q",
			status, state, memory.available/kibPerMiB, memory.total/kibPerMiB, memory.swapFree/kibPerMiB, memory.swapTotal/kibPerMiB, memory.shared/kibPerMiB, action)
	}
	return paused
}
