package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// eighths are the partial block characters, from one eighth of a cell to a
// full cell. They let a bar resolve to an eighth of a column, so a narrow
// gauge still moves when the value changes by a percent or two.
var eighths = [...]rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉', '█'}

// gauge renders a horizontal bar of the given width, filled in proportion to
// pct and coloured by its thresholds.
//
// The filled part is drawn with solid blocks and the remainder with a thin
// rule rather than a second block colour. On a terminal whose palette you do
// not control, a light rule reads as "empty" far more reliably than a dim
// block, which many themes render almost as bright as the fill.
func gauge(pct float64, width int, th thresholds) string {
	if width <= 0 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	style := th.styleFor(pct)

	// Work in eighths of a cell, then split into whole cells and a remainder.
	units := int(pct / 100 * float64(width) * 8)
	full := units / 8
	rem := units % 8
	if full > width {
		full, rem = width, 0
	}

	var b strings.Builder
	b.WriteString(strings.Repeat("█", full))
	if rem > 0 && full < width {
		b.WriteRune(eighths[rem-1])
	}
	bar := style.Render(b.String())

	used := full
	if rem > 0 && full < width {
		used++
	}
	if used < width {
		bar += styFaint.Render(strings.Repeat("─", width-used))
	}
	return bar
}

// meter renders a labelled gauge on one line:
//
//	CPU   ███████▍──────────  42%
//
// labelW and valueW are fixed so a column of meters aligns regardless of the
// values in it.
func meter(label string, pct float64, width, labelW, valueW int, th thresholds) string {
	barW := width - labelW - valueW - 2
	if barW < 1 {
		barW = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		styLabel.Render(pad(label, labelW)),
		" ",
		gauge(pct, barW, th),
		" ",
		th.styleFor(pct).Render(padLeft(percent(pct), valueW)),
	)
}
