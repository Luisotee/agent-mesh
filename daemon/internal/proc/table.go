package proc

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// Info describes one running process read from /proc.
type Info struct {
	PID        int
	PPID       int
	TTY        uint64
	Argv       []string
	Cwd        string
	StartTicks uint64
}

// Table reads the processes visible in /proc in one pass. A process that exits mid-scan is skipped; one whose
// cwd cannot be read has Cwd "".
func Table() ([]Info, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var table []Info
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		f, err := stat(pid)
		if err != nil {
			continue
		}
		ppid, err1 := strconv.Atoi(f[1])
		tty, err2 := strconv.ParseUint(f[4], 10, 64)
		ticks, err3 := strconv.ParseUint(f[19], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		rawCmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		var argv []string
		for _, part := range bytes.Split(rawCmd, []byte{0}) {
			if len(part) > 0 {
				argv = append(argv, string(part))
			}
		}
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		table = append(table, Info{
			PID:        pid,
			PPID:       ppid,
			TTY:        tty,
			Argv:       argv,
			Cwd:        cwd,
			StartTicks: ticks,
		})
	}
	return table, nil
}

// TaskIDs returns a process's thread IDs (/proc/<pid>/task); falls back to []int{pid} when unreadable.
func TaskIDs(pid int) []int {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return []int{pid}
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil && tid > 0 {
			tids = append(tids, tid)
		}
	}
	if len(tids) == 0 {
		return []int{pid}
	}
	return tids
}

// Tree returns pid followed by all of its descendants found in table.
func Tree(pid int, table []Info) []int {
	children := make(map[int][]int)
	for _, p := range table {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	var tree []int
	todo := []int{pid}
	seen := make(map[int]bool)
	for len(todo) > 0 {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		tree = append(tree, p)
		todo = append(todo, children[p]...)
	}
	return tree
}
