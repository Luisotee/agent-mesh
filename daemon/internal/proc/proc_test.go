package proc

import (
	"os"
	"os/exec"
	"slices"
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

func TestTableFindsSelfAndChild(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	table, err := Table()
	if err != nil {
		t.Fatal(err)
	}
	me := os.Getpid()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var foundSelf, foundChild *Info
	for i := range table {
		if table[i].PID == me {
			foundSelf = &table[i]
		}
		if table[i].PID == cmd.Process.Pid {
			foundChild = &table[i]
		}
	}
	if foundSelf == nil || foundSelf.Cwd != cwd || foundSelf.StartTicks == 0 {
		t.Fatalf("self in Table: %+v", foundSelf)
	}
	if foundChild == nil || foundChild.PPID != me || len(foundChild.Argv) < 1 || foundChild.Argv[0] != "sleep" {
		t.Fatalf("child in Table: %+v", foundChild)
	}
	tree := Tree(me, table)
	if !slices.Contains(tree, me) || !slices.Contains(tree, cmd.Process.Pid) {
		t.Errorf("Tree(%d) = %v, want self and child %d", me, tree, cmd.Process.Pid)
	}
	tids := TaskIDs(me)
	if !slices.Contains(tids, me) {
		t.Errorf("TaskIDs(%d) = %v, want %d included", me, tids, me)
	}
	if got := TaskIDs(1<<29 - 1); len(got) != 1 || got[0] != 1<<29-1 {
		t.Errorf("TaskIDs(missing) = %v", got)
	}
}

func TestTreeHandlesCycles(t *testing.T) {
	table := []Info{
		{PID: 10, PPID: 20},
		{PID: 20, PPID: 10},
		{PID: 30, PPID: 10},
		{PID: 40, PPID: 1},
	}
	tree := Tree(10, table)
	slices.Sort(tree)
	if !slices.Equal(tree, []int{10, 20, 30}) {
		t.Errorf("Tree with cycle = %v, want [10 20 30]", tree)
	}
}

func TestAffinityAndCPUList(t *testing.T) {
	cpus := MachineCPUs()
	if len(cpus) == 0 {
		t.Fatal("MachineCPUs returned empty")
	}
	if !slices.IsSorted(cpus) {
		t.Errorf("MachineCPUs not sorted: %v", cpus)
	}
	if _, err := GetAffinity(0); err != nil {
		t.Errorf("GetAffinity(0): %v", err)
	}
	if got := CPUList([]int{9, 0, 1, 2, 5, 7, 8}); got != "0-2,5,7-9" {
		t.Errorf("CPUList = %q, want 0-2,5,7-9", got)
	}
	if got := CPUList(nil); got != "" {
		t.Errorf("CPUList(nil) = %q, want empty", got)
	}
}
