package main

import (
	"fmt"
	"html/template"
	"slices"
	"time"
)

// Formatting for the web templates. internal/ui has equivalents, but they
// pad for fixed-width terminal columns and importing ui would carry Bubble
// Tea into this binary a stage early; these few lines are the cheaper copy.

var templateFuncs = template.FuncMap{
	"bytes":   fmtBytes,
	"rate":    fmtRate,
	"percent": fmtPercent,
	"ago":     fmtAgo,
	"uptime":  fmtUptime,
	"absent":  hasAbsent,
}

// fmtBytes renders a byte count in binary units.
func fmtBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := uint64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(v)/float64(div), "KMGTPE"[exp])
}

// fmtRate renders a bytes-per-second figure.
func fmtRate(v float64) string {
	if v < 0 {
		v = 0
	}
	return fmtBytes(uint64(v)) + "/s"
}

func fmtPercent(v float64) string {
	return fmt.Sprintf("%.0f%%", v)
}

// fmtAgo renders how long ago t was, coarsely: the fleet list wants "3m",
// not a timestamp to do arithmetic on.
func fmtAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// fmtUptime renders a seconds count the way the TUI does: largest two
// units.
func fmtUptime(secs int64) string {
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// hasAbsent reports whether field is in a report's absent list, which is
// what decides a dash instead of a number.
func hasAbsent(absent []string, field string) bool {
	return slices.Contains(absent, field)
}
