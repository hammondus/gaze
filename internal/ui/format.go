package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// binaryUnits are the suffixes for powers of 1024. Memory, disk, and network
// figures all use them, because that is what the kernel counts in and what
// every other tool on the machine shows.
var binaryUnits = [...]string{"B", "K", "M", "G", "T", "P"}

// bytes renders a byte count in at most five columns: "512B", "1.4K", "16G".
//
// Values below ten in a unit keep one decimal, larger ones drop it. That
// bounds the width without losing precision where it matters, since the
// difference between 1.4G and 1.9G is worth seeing and the difference between
// 812G and 813G is not.
func bytes(v uint64) string {
	f := float64(v)
	i := 0
	for f >= 1024 && i < len(binaryUnits)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", v)
	}
	if f < 10 {
		return fmt.Sprintf("%.1f%s", f, binaryUnits[i])
	}
	return fmt.Sprintf("%.0f%s", f, binaryUnits[i])
}

// rate renders a per-second byte rate, as in "1.4M/s".
func rate(v float64) string {
	if v < 1 {
		return "0/s"
	}
	return bytes(uint64(v)) + "/s"
}

// count renders a plain number with the same unit suffixes, for things that
// are not bytes such as IOPS or packet counts.
func count(v float64) string {
	if v < 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	for _, u := range []struct {
		div float64
		sfx string
	}{{1e9, "G"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= u.div {
			return fmt.Sprintf("%.1f%s", v/u.div, u.sfx)
		}
	}
	return fmt.Sprintf("%.0f", v)
}

// percent renders a percentage in four columns: "  0%" to "100%", with one
// decimal below ten so a busy-but-not-idle CPU is distinguishable from an idle
// one.
func percent(v float64) string {
	switch {
	case v <= 0:
		return "0%"
	case v < 10:
		return fmt.Sprintf("%.1f%%", v)
	default:
		return fmt.Sprintf("%.0f%%", v)
	}
}

// uptime renders a duration the way `uptime` does: "6d 4h 12m", dropping the
// units that are zero from the left.
func uptime(d time.Duration) string {
	if d <= 0 {
		return "unknown"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// cpuTime renders cumulative process CPU time as "MM:SS.hh", the column top
// and htop both show.
func cpuTime(d time.Duration) string {
	mins := int(d.Minutes())
	secs := d.Seconds() - float64(mins*60)
	if mins >= 60 {
		return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
	}
	return fmt.Sprintf("%d:%05.2f", mins, secs)
}

// pad right-pads a string to n display columns, truncating with an ellipsis if
// it is longer.
//
// Width is counted in runes rather than bytes so a non-ASCII process name or
// mount path does not throw the column alignment out.
func pad(s string, n int) string {
	w := utf8.RuneCountInString(s)
	if w == n {
		return s
	}
	if w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return truncate(s, n)
}

// padLeft left-pads a string to n columns, for right-aligned numeric columns.
func padLeft(s string, n int) string {
	w := utf8.RuneCountInString(s)
	if w >= n {
		return truncate(s, n)
	}
	return strings.Repeat(" ", n-w) + s
}

// truncate shortens a string to n display columns, marking the cut with "…".
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// elide shortens a long path from the left, keeping the end that identifies
// it: "…/lib/systemd/systemd-udevd". A path's tail carries the meaning, so
// cutting the head loses less than cutting the tail.
func elide(s string, n int) string {
	w := utf8.RuneCountInString(s)
	if w <= n || n <= 1 {
		return truncate(s, n)
	}
	r := []rune(s)
	return "…" + string(r[w-n+1:])
}

// trimZero renders a float to one decimal, dropping a trailing ".0" so a
// column of temperatures does not read as noisier than it is.
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// age renders a duration in at most six columns, for a container's uptime.
//
// The unit shrinks as the duration grows: seconds matter for something that
// just started, and days matter for something that has been up since the last
// reboot. Nothing between the two is worth six characters of precision.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
