package ui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/metrics"
)

// sortKey is the column the process table is ordered by.
type sortKey int

const (
	sortCPU sortKey = iota
	sortMem
	sortSwap
	sortTime
	sortPID
	sortName
	sortUser
)

// name is the label shown in the footer.
func (k sortKey) name() string {
	return [...]string{"cpu", "mem", "swap", "time", "pid", "name", "user"}[k]
}

// less orders two processes by the chosen column.
//
// Ties are the normal case, not the edge case: on an idle machine every
// process reads zero percent. Falling straight back to PID fills the screen
// with the low-numbered kernel threads and buries everything you started, so
// CPU breaks its ties on memory and memory breaks its ties on CPU. PID is the
// last resort, and it is always reached, so the order never shuffles between
// refreshes.
func (k sortKey) less(a, b metrics.Process) bool {
	switch k {
	case sortCPU:
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
		if a.RSS != b.RSS {
			return a.RSS > b.RSS
		}
	case sortMem:
		if a.RSS != b.RSS {
			return a.RSS > b.RSS
		}
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
	case sortSwap:
		if a.Swap != b.Swap {
			return a.Swap > b.Swap
		}
		// Most processes hold nothing in swap, so this column ties more than
		// any other. Resident size is the useful second key.
		if a.RSS != b.RSS {
			return a.RSS > b.RSS
		}
	case sortTime:
		if a.CPUTime != b.CPUTime {
			return a.CPUTime > b.CPUTime
		}
	case sortName:
		if a.Name != b.Name {
			return a.Name < b.Name
		}
	case sortUser:
		if a.User != b.User {
			return a.User < b.User
		}
	}
	return a.PID < b.PID
}

var plainProc = func(metrics.Process) lipgloss.Style { return styText }

// swapStyle marks a process that holds anything in swap.
//
// Swap is judged against the process, not against the machine: any swap at all
// means some of this process has been paged to disk, and a zero reads as
// nothing worth looking at. A threshold on a percentage would colour every row
// the same on a machine with no swap and every row red on one that is
// thrashing.
func swapStyle(p metrics.Process) lipgloss.Style {
	if p.Swap == 0 {
		return styFaint
	}
	return styWarn
}

// procTable is the process list.
var procTable = table[metrics.Process]{
	minFlex: 14,
	cols: []column[metrics.Process]{
		{"PID", 7, 0, int(sortPID), true, true,
			func(p metrics.Process) string { return strconv.Itoa(p.PID) }, plainProc},
		{"USER", 10, 70, int(sortUser), true, false,
			func(p metrics.Process) string { return p.User }, plainProc},
		{"CPU%", 6, 0, int(sortCPU), true, true,
			func(p metrics.Process) string { return percent(p.CPU) },
			func(p metrics.Process) lipgloss.Style { return thCPU.styleFor(p.CPU) }},
		{"MEM%", 6, 48, int(sortMem), true, true,
			func(p metrics.Process) string { return percent(p.MemPct) },
			func(p metrics.Process) lipgloss.Style { return thMem.styleFor(p.MemPct) }},
		{"RSS", 7, 56, int(sortMem), true, true,
			func(p metrics.Process) string { return bytes(p.RSS) }, plainProc},
		{"SWAP", 7, 96, int(sortSwap), true, true,
			func(p metrics.Process) string { return bytes(p.Swap) }, swapStyle},
		{"TIME+", 9, 88, int(sortTime), true, true,
			func(p metrics.Process) string { return cpuTime(p.CPUTime) }, plainProc},
		{"THR", 4, 104, 0, false, true,
			func(p metrics.Process) string { return strconv.Itoa(p.Threads) }, plainProc},
		{"S", 2, 76, 0, false, false,
			func(p metrics.Process) string { return string(p.State) },
			func(p metrics.Process) lipgloss.Style { return stateStyle(p.State) }},
	},
	flex: column[metrics.Process]{
		label: "COMMAND",
		value: func(p metrics.Process) string {
			if p.Cmdline != "" {
				return p.Cmdline
			}
			// A kernel thread has no command line, and neither does a zombie.
			// Bracketing the name is the convention ps uses for both.
			return "[" + p.Name + "]"
		},
		style: plainProc,
	},
}

// filterProcesses applies the visibility filters and the ordering.
//
// A kernel thread runs entirely inside the kernel: it holds no memory, appears
// as a bracketed name because it has no command line, and on a typical host
// outnumbers everything you started. Hiding it is a display choice, so it is
// made here and not in the collector.
func filterProcesses(procs []metrics.Process, filter string, key sortKey, hideKernel bool) []metrics.Process {
	needle := strings.ToLower(filter)
	out := make([]metrics.Process, 0, len(procs))
	for _, p := range procs {
		if hideKernel && p.Kernel {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(p.Name), needle) &&
			!strings.Contains(strings.ToLower(p.Cmdline), needle) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return key.less(out[i], out[j]) })
	return out
}

// stateStyle colours the process state letter. Only the states worth noticing
// get a colour: a stopped or zombie process is a problem, and a process in
// uninterruptible sleep is usually blocked on I/O that may not return.
func stateStyle(state byte) lipgloss.Style {
	switch state {
	case 'R':
		return styOK
	case 'D':
		return styWarn
	case 'Z', 'T', 't':
		return styCrit
	}
	return styText
}
