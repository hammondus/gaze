package ui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/metrics"
)

// ctrSort is the column the container table is ordered by. It reuses the
// process table's key letters where they mean the same thing.
type ctrSort int

const (
	ctrCPU ctrSort = iota
	ctrMem
	ctrUptime
	ctrName
	ctrIO
)

func (k ctrSort) name() string {
	return [...]string{"cpu", "mem", "uptime", "name", "io"}[k]
}

// less orders two containers.
//
// A container that is not running always sorts last, whatever the column. Its
// rates are all zero, so it would otherwise scatter through the list and push
// the running ones off the screen.
func (k ctrSort) less(a, b metrics.Container) bool {
	if ar, br := a.State == "running", b.State == "running"; ar != br {
		return ar
	}
	switch k {
	case ctrCPU:
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
		if a.MemUsed != b.MemUsed {
			return a.MemUsed > b.MemUsed
		}
	case ctrMem:
		if a.MemUsed != b.MemUsed {
			return a.MemUsed > b.MemUsed
		}
	case ctrUptime:
		if a.Uptime != b.Uptime {
			return a.Uptime > b.Uptime
		}
	case ctrIO:
		ai, bi := a.ReadRate+a.WriteRate, b.ReadRate+b.WriteRate
		if ai != bi {
			return ai > bi
		}
	}
	return a.Name < b.Name
}

var plainCtr = func(metrics.Container) lipgloss.Style { return styText }

// ctrStateStyle colours the state. A container that exited non-zero is the one
// worth spotting in a long list.
func ctrStateStyle(c metrics.Container) lipgloss.Style {
	switch c.State {
	case "running":
		return styOK
	case "paused", "restarting", "created":
		return styWarn
	case "exited", "dead":
		// The daemon's status reads "Exited (0) ..." for a clean exit, which
		// is ordinary, and any other code for a failure.
		if strings.HasPrefix(c.Status, "Exited (0)") {
			return styFaint
		}
		return styCrit
	}
	return styText
}

// ctrTable is the full container list, used by the split and container views.
var ctrTable = table[metrics.Container]{
	minFlex: 12,
	cols: []column[metrics.Container]{
		{"NAME", 20, 0, int(ctrName), true, false,
			func(c metrics.Container) string { return c.Name }, plainCtr},
		{"STATE", 9, 0, 0, false, false,
			func(c metrics.Container) string { return c.State }, ctrStateStyle},
		{"UPTIME", 7, 62, int(ctrUptime), true, true,
			func(c metrics.Container) string {
				if c.State != "running" {
					return "-"
				}
				return age(c.Uptime)
			}, plainCtr},
		{"CPU%", 6, 0, int(ctrCPU), true, true,
			func(c metrics.Container) string { return percent(c.CPU) },
			func(c metrics.Container) lipgloss.Style { return thCPU.styleFor(c.CPU) }},
		{"MEM", 7, 50, int(ctrMem), true, true,
			func(c metrics.Container) string { return bytes(c.MemUsed) }, plainCtr},
		{"IOR", 9, 74, int(ctrIO), true, true,
			func(c metrics.Container) string { return rate(c.ReadRate) }, plainCtr},
		{"IOW", 9, 76, int(ctrIO), true, true,
			func(c metrics.Container) string { return rate(c.WriteRate) }, plainCtr},
		{"RX", 9, 98, 0, false, true,
			func(c metrics.Container) string { return rate(c.RxRate) }, plainCtr},
		{"TX", 9, 100, 0, false, true,
			func(c metrics.Container) string { return rate(c.TxRate) }, plainCtr},
		{"MEM%", 6, 112, int(ctrMem), true, true,
			func(c metrics.Container) string { return percent(c.MemPct) },
			func(c metrics.Container) lipgloss.Style { return thMem.styleFor(c.MemPct) }},
		{"PIDS", 5, 124, 0, false, true,
			func(c metrics.Container) string {
				if c.State != "running" {
					return "-"
				}
				return strconv.Itoa(c.PIDs)
			}, plainCtr},
		{"IMAGE", 22, 150, 0, false, false,
			func(c metrics.Container) string { return c.Image }, plainCtr},
	},
	flex: column[metrics.Container]{
		label: "COMMAND",
		value: func(c metrics.Container) string { return c.Command },
		style: plainCtr,
	},
}

// filterContainers applies the name filter and the ordering.
//
// Containers that are not running only appear in the dedicated view. In the
// other two the container list shares the screen with everything else, and a
// row of dashes for something that exited last week earns none of that space.
func filterContainers(cs []metrics.Container, filter string, key ctrSort, showStopped bool) []metrics.Container {
	needle := strings.ToLower(filter)
	out := make([]metrics.Container, 0, len(cs))
	for _, c := range cs {
		if !showStopped && c.State != "running" {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(c.Name), needle) &&
			!strings.Contains(strings.ToLower(c.Image), needle) &&
			!strings.Contains(strings.ToLower(c.Command), needle) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return key.less(out[i], out[j]) })
	return out
}
