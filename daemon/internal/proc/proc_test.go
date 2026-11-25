package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestStartTicksIdentifiesAProcess(t *testing.T) {
	me, err := StartTicks(os.Getpid())
	if err != nil || me == 0 {
		t.Fatalf("StartTicks(self) = %d, %v", me, err)
	}
	if !Same(os.Getpid(), me) || Same(os.Getpid(), me+1) {
		t.Error("Same disagrees with StartTicks")
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ticks, err := StartTicks(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// A wrong start time (a reused pid) is never signalled.
	if err := SignalGroup(cmd.Process.Pid, ticks+1, 0, syscall.SIGKILL); err != ErrGone {
		t.Errorf("signalled a process with another start time: %v", err)
	}
	if err := SignalGroup(cmd.Process.Pid, ticks, 0, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not killed")
	}
	if err := SignalGroup(cmd.Process.Pid, ticks, 0, syscall.SIGKILL); err != ErrGone {
		t.Errorf("signalled a dead process: %v", err)
	}
}
