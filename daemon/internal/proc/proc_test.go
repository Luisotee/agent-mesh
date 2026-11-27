package proc

import (
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
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

// A command that ran and ended between two samples is in its parent's waited-for children times: Usage counts
// them, so a worker whose CPU goes into short builds and test runs doesn't read as idle.
func TestUsageCountsTheCPUOfFinishedChildren(t *testing.T) {
	_, before, err := Usage(os.Getpid())
	if err != nil {
		t.Skip("no /proc")
	}
	if out, err := exec.Command("sh", "-c", "i=0; while [ $i -lt 600000 ]; do i=$((i+1)); done").CombinedOutput(); err != nil {
		t.Fatalf("the child: %v %s", err, out)
	}
	_, after, err := Usage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if after-before < 5 { // at least 50 ms of the child CPU
		t.Errorf("CPU ticks went from %d to %d: the finished child was not counted", before, after)
	}
}

// A shell's background job has a process group of its own but the shell's session: SignalSession reaches it, and
// leaves the shell (the leader's own group) to SignalGroup.
func TestSignalSessionReachesJobsOutsideTheLeadersGroup(t *testing.T) {
	cmd := exec.Command("bash", "-c", "set -m; sleep 4343 & echo JOB=$!; wait")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	buf := make([]byte, 0, 256)
	one := make([]byte, 64)
	var job int
	for deadline := time.Now().Add(10 * time.Second); job == 0 && time.Now().Before(deadline); {
		n, _ := ptmx.Read(one)
		buf = append(buf, one[:n]...)
		if _, rest, ok := strings.Cut(string(buf), "JOB="); ok && strings.Contains(rest, "\n") {
			job, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	if job <= 1 {
		t.Fatalf("no job pid in %q", buf)
	}
	t.Cleanup(func() { _ = syscall.Kill(job, syscall.SIGKILL) })
	tty, err := TTY(cmd.Process.Pid)
	if err != nil || tty == 0 {
		t.Fatalf("TTY = %d, %v", tty, err)
	}
	if f, err := stat(job); err != nil || f[2] == strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("the job should have a group of its own: %v, %v", f, err)
	}
	// A pid that leads no session, or no terminal, signals nothing.
	SignalSession(1, tty, syscall.SIGKILL)
	SignalSession(cmd.Process.Pid, 0, syscall.SIGKILL)
	SignalSession(job, tty, syscall.SIGKILL) // the job leads no session: nothing has its session id
	if f, err := stat(job); err != nil || f[0] == "Z" {
		t.Fatalf("the job was signalled: %v, %v", f, err)
	}
	SignalSession(cmd.Process.Pid, tty, syscall.SIGKILL)
	gone := func() bool {
		f, err := stat(job)
		return err != nil || f[0] == "Z"
	}
	for deadline := time.Now().Add(5 * time.Second); !gone() && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if !gone() {
		t.Error("the background job is still running")
	}
	// The shell is not signalled (its `wait` ends because the job did): it exits by itself, not on a signal.
	_ = cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signaled() {
		t.Errorf("the leader was signalled: %v", cmd.ProcessState)
	}
}
