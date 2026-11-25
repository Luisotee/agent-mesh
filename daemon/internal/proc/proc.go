// Package proc identifies processes by pid and start time, so a recorded pid is never signalled once the
// kernel has given it to another process.
package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrGone means the process is not the one recorded: it exited, or its pid was reused.
var ErrGone = errors.New("process is gone or its pid was reused")

// stat returns /proc/<pid>/stat from field 3 on: fields[0] is field 3 (state), so field n is fields[n-3].
func stat(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	// The command name (field 2) is in parentheses and may contain spaces or parentheses itself.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil, fmt.Errorf("unparseable /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return nil, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return fields, nil
}

// StartTicks is a process's start time in clock ticks since boot (/proc/<pid>/stat field 22).
func StartTicks(pid int) (uint64, error) {
	f, err := stat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// TTY is a process's controlling terminal (field 7, tty_nr); 0 for none.
func TTY(pid int) (uint64, error) {
	f, err := stat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(f[4], 10, 64)
}

// groupOnTTY reports whether some live process is in process group pgid on terminal tty: then pgid is
// still our group, even with its leader gone (the kernel doesn't reuse a pid that names a live group).
func groupOnTTY(pgid int, tty uint64) bool { return GroupAlive(pgid, tty) }

// GroupAlive reports whether a process of group pgid runs on terminal tty.
func GroupAlive(pgid int, tty uint64) bool {
	if tty == 0 {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	want := strconv.Itoa(pgid)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		f, err := stat(pid)
		if err == nil && f[2] == want && f[4] == strconv.FormatUint(tty, 10) {
			return true
		}
	}
	return false
}

// Same reports whether pid still is the process that started at startTicks.
func Same(pid int, startTicks uint64) bool {
	t, err := StartTicks(pid)
	return err == nil && t == startTicks
}

// SignalGroup signals the process group pid leads, after checking it is still ours: its leader is the
// recorded process, or, once the leader exited, a member of the group still runs on the group's terminal.
func SignalGroup(pid int, startTicks, tty uint64, sig syscall.Signal) error {
	if pid <= 1 || !(Same(pid, startTicks) || groupOnTTY(pid, tty)) {
		return ErrGone
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrGone
		}
		return err
	}
	return nil
}

// Signal signals one process, after checking pid is still the recorded process.
func Signal(pid int, startTicks uint64, sig syscall.Signal) error {
	if pid <= 1 || !Same(pid, startTicks) {
		return ErrGone
	}
	return syscall.Kill(pid, sig)
}
