package ui

import (
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestBytes(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{10 * 1024, "10K"},
		{1024 * 1024, "1.0M"},
		{16 * 1024 * 1024 * 1024, "16G"},
		{1024 * 1024 * 1024 * 1024, "1.0T"},
	} {
		if got := bytes(c.in); got != c.want {
			t.Errorf("bytes(%d) = %q, want %q", c.in, got, c.want)
		}
		if w := len(bytes(c.in)); w > 5 {
			t.Errorf("bytes(%d) is %d columns wide, want at most 5", c.in, w)
		}
	}
}

func TestUptime(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{0, "unknown"},
		{45 * time.Second, "0m"},
		{90 * time.Minute, "1h 30m"},
		{50 * time.Hour, "2d 2h 0m"},
	} {
		if got := uptime(c.in); got != c.want {
			t.Errorf("uptime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPadCountsRunesNotBytes(t *testing.T) {
	// A multi-byte name must not push the columns beside it out of line.
	if got := pad("héllo", 8); lipgloss.Width(got) != 8 {
		t.Errorf("pad width = %d, want 8: %q", lipgloss.Width(got), got)
	}
	if got := truncate("héllo world", 7); lipgloss.Width(got) != 7 {
		t.Errorf("truncate width = %d, want 7: %q", lipgloss.Width(got), got)
	}
}

func TestElideKeepsTheTail(t *testing.T) {
	// The end of a path is what identifies it, so the head is what goes.
	if got := elide("/usr/lib/systemd/systemd-udevd", 12); got != "…ystemd-udevd" && got != "…stemd-udevd" {
		t.Errorf("elide = %q", got)
	}
	if got := elide("/short", 12); got != "/short" {
		t.Errorf("elide left a short path alone: %q", got)
	}
}

func TestGaugeWidth(t *testing.T) {
	// A gauge must occupy exactly the width it was given at every value, or
	// the column to its right moves as the number changes.
	for _, pct := range []float64{-5, 0, 0.4, 1, 33.3, 50, 99.9, 100, 150} {
		g := gauge(pct, 20, thCPU)
		if w := lipgloss.Width(g); w != 20 {
			t.Errorf("gauge(%.1f) width = %d, want 20", pct, w)
		}
	}
	if lipgloss.Width(gauge(50, 0, thCPU)) != 0 {
		t.Error("a zero-width gauge must render nothing")
	}
}

func TestMeterWidth(t *testing.T) {
	for _, w := range []int{20, 40, 80} {
		if got := lipgloss.Width(meter("CPU", 42, w, 5, 6, thCPU)); got != w {
			t.Errorf("meter width = %d, want %d", got, w)
		}
	}
}

func TestRingKeepsOrderAndWraps(t *testing.T) {
	r := newRing(3)
	r.push(1)
	r.push(2)
	if got := r.values(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("partial ring = %v", got)
	}
	r.push(3)
	r.push(4) // evicts 1
	if got := r.values(); len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Errorf("wrapped ring = %v, want [2 3 4]", got)
	}
	if r.max() != 4 {
		t.Errorf("max = %v, want 4", r.max())
	}
}

func TestSparkWidthAndAlignment(t *testing.T) {
	r := newRing(10)
	r.push(50)
	r.push(100)
	s := r.spark(8, 100, styOK)
	if w := lipgloss.Width(s); w != 8 {
		t.Errorf("spark width = %d, want 8", w)
	}
	// A short history is right-aligned, so the newest sample sits at the edge.
	if plain := stripStyle(s); plain[:6] != "      " {
		t.Errorf("spark is not right-aligned: %q", plain)
	}
	// An empty history must not divide by a zero peak.
	if w := lipgloss.Width(newRing(4).spark(6, 0, styOK)); w != 6 {
		t.Errorf("empty spark width = %d, want 6", w)
	}
}

// stripStyle renders a string with styling disabled, so a test can look at the
// characters without the escape sequences around them.
func stripStyle(s string) string {
	return lipgloss.NewStyle().Render(s)
}
