//go:build !windows

package localruntime

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestIssueProcessesRequireConfirmedTermination(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	record := IssueClaimRecord{ProcessGroups: []int{cmd.Process.Pid}}
	if stopped, err := IssueProcessesStopped(record); err != nil || stopped {
		t.Fatalf("live group considered stopped: %v %v", stopped, err)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if stopped, err := IssueProcessesStopped(record); err != nil || !stopped {
		t.Fatalf("exited group considered live: %v %v", stopped, err)
	}
	if stopped, err := IssueProcessesStopped(IssueClaimRecord{ProcessUncertain: true}); err == nil || stopped {
		t.Fatal("ambiguous process launch automatically recovered")
	}
}
