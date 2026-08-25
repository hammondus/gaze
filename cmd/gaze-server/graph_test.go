package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/report"
)

var t0 = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// minutely builds n evenly spaced points, one per minute from t0.
func minutely(n int) []gpoint {
	out := make([]gpoint, n)
	for i := range out {
		out[i] = gpoint{t: t0.Add(time.Duration(i) * time.Minute), min: 10, max: 30, mean: 20, weight: 6}
	}
	return out
}

// TestGapsBreakTheLine is the rule the whole file exists for: a host that
// went silent must show a hole, never a line drawn across it.
func TestGapsBreakTheLine(t *testing.T) {
	points := minutely(30)
	// A ten-minute outage between minute 9 and 19.
	points = append(points[:10], points[20:]...)

	segs := segments(points)
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if len(segs[0]) != 10 || len(segs[1]) != 10 {
		t.Fatalf("segment sizes = %d, %d", len(segs[0]), len(segs[1]))
	}

	g := buildGraph("cpu", t0, t0.Add(30*time.Minute), 100, fmtYPercent, true,
		series{class: "a", points: points})
	if got := strings.Count(g.Lines[0].D, "M"); got != 2 {
		t.Fatalf("line has %d subpaths, want 2", got)
	}
	if got := strings.Count(g.Band, "M"); got != 2 {
		t.Fatalf("band has %d subpaths, want 2", got)
	}
}

// TestSlowIntervalIsNotAGap: the threshold comes from the data's own
// spacing, so a host reporting every five minutes still draws one line.
func TestSlowIntervalIsNotAGap(t *testing.T) {
	var points []gpoint
	for i := range 20 {
		points = append(points, gpoint{t: t0.Add(time.Duration(i) * 5 * time.Minute), mean: 20, weight: 6})
	}
	if segs := segments(points); len(segs) != 1 {
		t.Fatalf("segments = %d, want 1", len(segs))
	}
}

// TestAbsentPointsAreGaps: a skipped point contributes nothing, not zero.
func TestAbsentPointsAreGaps(t *testing.T) {
	points := minutely(30)
	for i := 10; i < 20; i++ {
		points[i].skip = true
	}
	if segs := segments(points); len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}

	// All absent: the graph says so rather than drawing a zero line.
	for i := range points {
		points[i].skip = true
	}
	g := buildGraph("swap", t0, t0.Add(30*time.Minute), 0, fmtYBytes, true,
		series{class: "a", points: points})
	if g.Note != "no data" || len(g.Lines) != 0 || g.Band != "" {
		t.Fatalf("all-absent graph = %+v", g)
	}
}

// TestIsolatedPointBecomesADot: a lone observation between gaps cannot
// carry a line segment, and must still be visible.
func TestIsolatedPointBecomesADot(t *testing.T) {
	// One point at minute 0, silence, then a run from minute 10: the run
	// sets the usual spacing, the lone point sits past the threshold.
	points := minutely(20)[10:]
	points = append([]gpoint{{t: t0, mean: 20, weight: 6}}, points...)

	g := buildGraph("cpu", t0, t0.Add(30*time.Minute), 100, fmtYPercent, false,
		series{class: "a", points: points})
	if len(g.Dots) != 1 {
		t.Fatalf("dots = %d, want 1", len(g.Dots))
	}
}

// TestThinBounds: ten thousand raw points must come out under the cap with
// the envelope intact — min of mins, max of maxes, weighted mean.
func TestThinBounds(t *testing.T) {
	points := minutely(10080) // a raw week
	points[5000].max = 99     // one spike, which must survive

	thinned := thin(points, maxGraphPoints)
	if len(thinned) > maxGraphPoints {
		t.Fatalf("thinned to %d, cap is %d", len(thinned), maxGraphPoints)
	}
	var peak float64
	for _, p := range thinned {
		peak = max(peak, p.max)
	}
	if peak != 99 {
		t.Fatalf("the spike was averaged away: peak = %v", peak)
	}
	if thinned[0].mean != 20 {
		t.Fatalf("weighted mean = %v, want 20", thinned[0].mean)
	}
}

// TestGraphStaysInsideTheViewBox: every emitted coordinate lands in the
// plot area, whatever the data does — the SVG equivalent of the TUI's
// frame-fits tests.
func TestGraphStaysInsideTheViewBox(t *testing.T) {
	points := minutely(30)
	points[3].max = 1e9 // wild outlier against a fixed 0..100 scale
	points[4].min = -50

	g := buildGraph("cpu", t0, t0.Add(30*time.Minute), 100, fmtYPercent, true,
		series{class: "a", points: points})

	coord := regexp.MustCompile(`[-0-9.]+`)
	for _, d := range []string{g.Band, g.Lines[0].D} {
		nums := coord.FindAllString(d, -1)
		for i := 0; i+1 < len(nums); i += 2 {
			x, _ := strconv.ParseFloat(nums[i], 64)
			y, _ := strconv.ParseFloat(nums[i+1], 64)
			if x < gPadL-0.01 || x > gWidth-gPadR+0.01 {
				t.Fatalf("x = %v outside plot", x)
			}
			if y < gPadT-0.01 || y > gHeight-gPadB+0.01 {
				t.Fatalf("y = %v outside plot", y)
			}
		}
	}
}

func TestNiceCeil(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0.7, 1}, {1, 1}, {1.2, 2}, {3, 5}, {7, 10}, {42, 50}, {160, 200}, {900, 1000},
	} {
		if got := niceCeil(tc.in); got != tc.want {
			t.Errorf("niceCeil(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestScalarGraphs covers the converters: swap distinguishes "absent" (a
// gap) from "no swap" (a note), and memory scales to the machine's total.
func TestScalarGraphs(t *testing.T) {
	var points []query.Point
	for i := range 10 {
		points = append(points, query.Point{
			Start: t0.Add(time.Duration(i) * time.Minute), Samples: 6,
			CPU: report.Stat{Min: 10, Max: 30, Mean: 20},
			Mem: report.Stat{Min: 1 << 30, Max: 2 << 30, Mean: 3 << 29}, MemTotal: 8 << 30,
		})
	}
	gs := scalarGraphs(points, t0, t0.Add(10*time.Minute))
	if len(gs) != 4 {
		t.Fatalf("graphs = %d, want cpu, load, memory, swap", len(gs))
	}
	swap := gs[3]
	if swap.Note != "no swap" {
		t.Fatalf("swapless machine's graph note = %q, want no swap", swap.Note)
	}

	// The same machine with swap absent (a future non-Linux host): gaps,
	// not a note and not a zero line.
	for i := range points {
		points[i].SwapTotal = 4 << 30
		points[i].Absent = []string{"swap"}
	}
	swap = scalarGraphs(points, t0, t0.Add(10*time.Minute))[3]
	if swap.Note != "no data" {
		t.Fatalf("absent swap note = %q, want no data", swap.Note)
	}

	// Memory's axis top is the machine's total, so a half-used machine
	// draws half-full.
	mem := gs[2]
	top := mem.YTicks[len(mem.YTicks)-1].Label
	if want := fmtBytes(8 << 30); top != want {
		t.Fatalf("memory axis top = %q, want %q", top, want)
	}
}

func TestXTickLayouts(t *testing.T) {
	x := func(time.Time) float64 { return 0 }
	if ticks := xTicks(t0, t0.Add(time.Hour), x); !regexp.MustCompile(`^\d\d:\d\d$`).MatchString(ticks[0].Label) {
		t.Errorf("1h tick = %q", ticks[0].Label)
	}
	if ticks := xTicks(t0, t0.Add(30*24*time.Hour), x); !strings.Contains(ticks[0].Label, "Aug") {
		t.Errorf("30d tick = %q", ticks[0].Label)
	}
}
