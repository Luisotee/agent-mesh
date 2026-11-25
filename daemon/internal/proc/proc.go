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

// StartTicks is a process's start time in clock ticks since boot (/proc/<pid>/stat field 22).
func StartTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name (field 2) is in parentheses and may contain spaces or parentheses itself.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unparseable /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state), so field 22 is fields[19].
	if len(fields) < 20 {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

// Same reports whether pid still is the process that started at startTicks.
func Same(pid int, startTicks uint64) bool {
	t, err := StartTicks(pid)
	return err == nil && t == startTicks
}

// SignalGroup signals the process group led by pid, after checking pid is still the recorded process.
func SignalGroup(pid int, startTicks uint64, sig syscall.Signal) error {
	if pid <= 1 || !Same(pid, startTicks) {
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
