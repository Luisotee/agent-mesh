package proc

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const cpuSetBits = 1024

// GetAffinity returns the sorted CPU IDs in pid's (or tid's; 0 for current thread) affinity mask.
func GetAffinity(pid int) ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(pid, &set); err != nil {
		return nil, err
	}
	want := set.Count()
	cpus := make([]int, 0, want)
	for cpu := 0; cpu < cpuSetBits && len(cpus) < want; cpu++ {
		if set.IsSet(cpu) {
			cpus = append(cpus, cpu)
		}
	}
	return cpus, nil
}

// SetAffinity sets the CPU affinity mask of thread tid (0 for current thread) to cpus.
func SetAffinity(tid int, cpus []int) error {
	var set unix.CPUSet
	for _, cpu := range cpus {
		if cpu < 0 || cpu >= cpuSetBits {
			return fmt.Errorf("cpu %d out of range [0, %d)", cpu, cpuSetBits)
		}
		set.Set(cpu)
	}
	return unix.SchedSetaffinity(tid, &set)
}

// MachineCPUs returns the cores available to the machine: pid 1's affinity mask (which no agent pin narrows),
// falling back to this process's own mask.
func MachineCPUs() []int {
	for _, pid := range []int{1, 0} {
		if cpus, err := GetAffinity(pid); err == nil && len(cpus) > 0 {
			return cpus
		}
	}
	return nil
}

// CPUList formats sorted CPU IDs into taskset range syntax (e.g. [9, 0, 1, 2, 5] -> "0-2,5,9").
func CPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	sorted := append([]int(nil), cpus...)
	sort.Ints(sorted)
	type span struct{ a, b int }
	var spans []span
	for _, c := range sorted {
		if n := len(spans); n > 0 && c == spans[n-1].b+1 {
			spans[n-1].b = c
		} else if n == 0 || c != spans[n-1].b {
			spans = append(spans, span{c, c})
		}
	}
	parts := make([]string, len(spans))
	for i, s := range spans {
		if s.b > s.a {
			parts[i] = fmt.Sprintf("%d-%d", s.a, s.b)
		} else {
			parts[i] = strconv.Itoa(s.a)
		}
	}
	return strings.Join(parts, ",")
}
