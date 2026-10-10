package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/hammondus/gaze/internal/threshold"
)

// The palette is adaptive: lipgloss picks the first value on a dark terminal
// and the second on a light one. Terminal themes vary too much for a single
// hard-coded set to stay readable on both.
var (
	colAccent = lipgloss.AdaptiveColor{Dark: "51", Light: "31"}   // panel titles
	colMuted  = lipgloss.AdaptiveColor{Dark: "245", Light: "244"} // labels, units
	colFaint  = lipgloss.AdaptiveColor{Dark: "238", Light: "252"} // rules, gauge track
	colText   = lipgloss.AdaptiveColor{Dark: "252", Light: "236"}
	colOK     = lipgloss.AdaptiveColor{Dark: "78", Light: "28"}
	colWarn   = lipgloss.AdaptiveColor{Dark: "221", Light: "130"}
	colCrit   = lipgloss.AdaptiveColor{Dark: "203", Light: "160"}
	colHeadBG = lipgloss.AdaptiveColor{Dark: "24", Light: "153"}
)

var (
	styTitle  = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styLabel  = lipgloss.NewStyle().Foreground(colMuted)
	styText   = lipgloss.NewStyle().Foreground(colText)
	styFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styOK     = lipgloss.NewStyle().Foreground(colOK)
	styWarn   = lipgloss.NewStyle().Foreground(colWarn)
	styCrit   = lipgloss.NewStyle().Foreground(colCrit)
	styHeader = lipgloss.NewStyle().Background(colHeadBG).Foreground(colText).Bold(true)
	styKey    = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styRow    = lipgloss.NewStyle().Foreground(colText)
	styRowSel = lipgloss.NewStyle().Foreground(colText).Background(colFaint)
)

// thresholds are the warning and critical points for one kind of metric.
// They live in one place so the colour of a number never disagrees with the
// colour of the bar beside it.
type thresholds struct{ warn, crit float64 }

// The four that have an alert rule come from the shared table, so a red
// number here is one the server alerts on. Load and temperature have no rule
// and stay local.
var (
	thCPU  = shared(threshold.CPU)
	thMem  = shared(threshold.Memory)
	thSwap = shared(threshold.Swap)
	thDisk = shared(threshold.Disk)
	thLoad = thresholds{70, 100} // as a percentage of core count
	thTemp = thresholds{70, 85}  // used only when the chip publishes none
)

func shared(t threshold.T) thresholds { return thresholds{t.Warn, t.Crit} }

// styleFor picks the colour for a value against its thresholds.
func (t thresholds) styleFor(v float64) lipgloss.Style {
	switch {
	case v >= t.crit:
		return styCrit
	case v >= t.warn:
		return styWarn
	default:
		return styOK
	}
}
