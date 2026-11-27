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

// SignalSession signals the processes of the session pid leads that run on terminal tty and are not in the leader's own
// process group: a shell's background jobs, which a shell with job control puts in groups of their own. Ending a terminal
// reaches them this way and not only through the shell passing the signal on: a bash that gets SIGHUP in the moment after
// it ran a command (before it is back at its prompt) was seen to exit without telling its jobs. Call it before signalling
// the group: once the leader has gone the session has no terminal any more (tty_nr 0) and nothing matches. The kernel
// doesn't reuse a pid that names a live session, so a session of this pid on this terminal is ours.
func SignalSession(pid int, tty uint64, sig syscall.Signal) {
	if pid <= 1 || tty == 0 {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	sid, term := strconv.Itoa(pid), strconv.FormatUint(tty, 10)
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p <= 1 {
			continue
		}
		// fields: state, ppid, pgrp, session, tty_nr
		if f, err := stat(p); err == nil && f[3] == sid && f[4] == term && f[2] != sid {
			_ = syscall.Kill(p, sig)
		}
	}
}

// SignalSessionMembers signals every process of session sid but its leader, by the session id alone, and returns how
// many it signalled. It is for the moments after a terminal's end was signalled: a job still between fork and exec then
// runs its shell's signal handler, which swallows a SIGHUP, and a session whose leader has gone has no terminal any
// more (tty_nr 0), so SignalSession matches nothing. The kernel doesn't reuse a pid that names a live session, so a
// process of session sid is the terminal's own.
func SignalSessionMembers(sid int, sig syscall.Signal) int {
	if sid <= 1 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want, n := strconv.Itoa(sid), 0
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p <= 1 || p == sid {
			continue
		}
		// fields: state, ppid, pgrp, session
		if f, err := stat(p); err == nil && f[3] == want && f[0] != "Z" {
			if syscall.Kill(p, sig) == nil {
				n++
			}
		}
	}
	return n
}

// Signal signals one process, after checking pid is still the recorded process.
func Signal(pid int, startTicks uint64, sig syscall.Signal) error {
	if pid <= 1 || !Same(pid, startTicks) {
		return ErrGone
	}
	return syscall.Kill(pid, sig)
}

// ClockTicks is the kernel's USER_HZ: the unit of the CPU times in /proc/<pid>/stat (100 on every Linux the
// daemon runs on).
const ClockTicks = 100

// Usage is a process's resident memory in kB (/proc/<pid>/statm) and the CPU time it has used, user plus system,
// in clock ticks (/proc/<pid>/stat fields 14 and 15), with that of the children it has waited for (fields 16 and
// 17): a build or a test run that starts and ends between two samples is only ever seen there.
func Usage(pid int) (rssKB, cpuTicks uint64, err error) {
	f, err := stat(pid)
	if err != nil {
		return 0, 0, err
	}
	var ticks uint64
	for _, i := range []int{11, 12, 13, 14} { // utime, stime, cutime, cstime
		n, perr := strconv.ParseUint(f[i], 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("unparseable CPU times in /proc/%d/stat", pid)
		}
		ticks += n
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, 0, err
	}
	m := strings.Fields(string(b))
	if len(m) < 2 {
		return 0, 0, fmt.Errorf("short /proc/%d/statm", pid)
	}
	pages, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return pages * uint64(os.Getpagesize()) / 1024, ticks, nil
}

// PPID is a process's parent (/proc/<pid>/stat field 4).
func PPID(pid int) (int, error) {
	f, err := stat(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(f[1])
}
