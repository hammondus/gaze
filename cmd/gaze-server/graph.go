package main

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Server-side SVG graphs. Go computes the geometry — scales, tick
// positions, path data — and the template stamps it into <svg> elements, so
// the numbers are testable without a browser. The one script on the page,
// static/hover.js, reads the readout each graph carries in a data
// attribute and draws a crosshair; it computes nothing.
//
// Gaps are the design centre: a host that stopped reporting must show a
// hole, and an absent field must show nothing at all. A line is therefore a
// list of disconnected segments, never one polyline interpolated across
// silence.

// Graph geometry, in viewBox units. The SVG scales to its container; these
// only set the aspect and the label sizing.
const (
	gWidth  = 600
	gHeight = 150
	gPadL   = 52 // room for y labels
	gPadR   = 8
	gPadT   = 6
	gPadB   = 18 // room for x labels
)

// maxGraphPoints bounds the points per series. A week of raw rows is ten
// thousand; a path that long is a megabyte of markup no screen can resolve,
// so buckets are merged the way the roll-up merges them.
const maxGraphPoints = 400

// gpoint is one observation on a graph's time axis.
type gpoint struct {
	t              time.Time
	min, max, mean float64
	weight         int    // samples behind the observation, for thinning
	skip           bool   // the platform could not supply this point
	note           string // extra readout text, such as "69.2 of 100.0 GiB"
}

// series is one line on a graph.
type series struct {
	class  string // CSS class of the line
	name   string // readout prefix, such as "rx"; empty for a lone series
	points []gpoint
}

// hpoint is one entry of a graph's hover readout: where the crosshair
// goes, and the lines the readout shows there.
type hpoint struct {
	X     float64
	Lines []string
}

// MarshalJSON writes an hpoint as a flat array, [x, line, line, ...],
// with x to one decimal. A graph carries a few hundred of these in an
// attribute, where every quote costs five bytes once escaped, so the
// keys and nesting an object would add are not worth having.
func (h hpoint) MarshalJSON() ([]byte, error) {
	flat := make([]any, 0, 1+len(h.Lines))
	flat = append(flat, math.Round(h.X*10)/10)
	for _, l := range h.Lines {
		flat = append(flat, l)
	}
	return json.Marshal(flat)
}

// graph is what the template draws.
type graph struct {
	Title  string
	Note   string // drawn instead of the series: "no data", "absent"
	Band   string // path d of the min–max envelope, "" if none
	Lines  []gline
	Dots   []gdot // isolated points no segment can carry
	XTicks []tick
	YTicks []tick
	// Hover is the readout as JSON, a list of hpoint, carried in a data
	// attribute for static/hover.js. Empty when there is nothing to show.
	Hover string
}

type gline struct {
	D     string
	Class string
}

type gdot struct {
	X, Y  float64
	Class string
}

type tick struct {
	X, Y  float64
	Label string
}

// buildGraph lays out one graph over [from, to]. yMax fixes the scale (0
// means scale from the data), fmtY renders axis labels, and band draws the
// first series' min–max envelope behind its mean line.
func buildGraph(title string, from, to time.Time, yMax float64, fmtY func(float64) string, band bool, ss ...series) graph {
	g := graph{Title: title}

	for i := range ss {
		ss[i].points = thin(ss[i].points, maxGraphPoints)
	}

	// The y scale covers the data unless the caller fixed it. Envelope
	// maxima count, so a spike the band shows is never clipped.
	top := yMax
	if top == 0 {
		for _, s := range ss {
			for _, p := range s.points {
				if !p.skip {
					top = max(top, p.max, p.mean)
				}
			}
		}
		top = niceCeil(top)
	}
	if top == 0 {
		top = 1 // a flat-zero series still needs a scale to draw on
	}

	plotW := float64(gWidth - gPadL - gPadR)
	plotH := float64(gHeight - gPadT - gPadB)
	span := to.Sub(from).Seconds()
	x := func(t time.Time) float64 {
		return gPadL + t.Sub(from).Seconds()/span*plotW
	}
	y := func(v float64) float64 {
		frac := min(max(v/top, 0), 1)
		return gPadT + (1-frac)*plotH
	}

	any := false
	for _, s := range ss {
		segs := segments(s.points)
		if len(segs) == 0 {
			continue
		}
		any = true
		if band && s.class == ss[0].class {
			g.Band = bandPath(segs, x, y)
		}
		var d strings.Builder
		for _, seg := range segs {
			if len(seg) == 1 {
				g.Dots = append(g.Dots, gdot{X: x(seg[0].t), Y: y(seg[0].mean), Class: s.class})
				continue
			}
			for i, p := range seg {
				if i == 0 {
					fmt.Fprintf(&d, "M%.1f %.1f", x(p.t), y(p.mean))
				} else {
					fmt.Fprintf(&d, "L%.1f %.1f", x(p.t), y(p.mean))
				}
			}
		}
		if d.Len() > 0 {
			g.Lines = append(g.Lines, gline{D: d.String(), Class: s.class})
		}
	}
	if !any {
		g.Note = "no data"
		return g
	}

	for _, frac := range []float64{0, 0.5, 1} {
		g.YTicks = append(g.YTicks, tick{Y: y(top * frac), Label: fmtY(top * frac)})
	}
	g.XTicks = xTicks(from, to, x)
	g.Hover = hoverJSON(hover(ss, fmtY, x))
	return g
}

// hover builds the readout from the thinned series. Every series on a
// graph comes from the same rows, so the first series' points set the
// positions and the others are matched by index when their times agree.
func hover(ss []series, fmtY func(float64) string, x func(time.Time) float64) []hpoint {
	if len(ss) == 0 {
		return nil
	}
	out := make([]hpoint, 0, len(ss[0].points))
	for i, p := range ss[0].points {
		h := hpoint{X: x(p.t), Lines: []string{readoutTime(p)}}
		for _, s := range ss {
			if i >= len(s.points) || !s.points[i].t.Equal(p.t) {
				continue
			}
			h.Lines = append(h.Lines, readoutValue(s.name, s.points[i], fmtY))
		}
		out = append(out, h)
	}
	return out
}

// readoutTime is the first line of a readout: when the bucket starts, in
// the server's zone, and how many samples stand behind it.
func readoutTime(p gpoint) string {
	t := p.t.Local().Format("2006-01-02 15:04")
	switch {
	case p.skip:
		return t
	case p.weight == 1:
		return t + " · 1 sample"
	default:
		return fmt.Sprintf("%s · %d samples", t, p.weight)
	}
}

// readoutValue is one series' line: the mean, with the min and max after
// it when they differ, so a raw point reads as one number and a rolled-up
// bucket shows its spread. Differ as labels, not as floats: a spread the
// axis format cannot show would read as "67% (67% to 67%)".
func readoutValue(name string, p gpoint, fmtY func(float64) string) string {
	var b strings.Builder
	if name != "" {
		b.WriteString(name + " ")
	}
	if p.skip {
		b.WriteString("no data")
		return b.String()
	}
	b.WriteString(fmtY(p.mean))
	if lo, hi := fmtY(p.min), fmtY(p.max); lo != hi {
		fmt.Fprintf(&b, " (%s to %s)", lo, hi)
	}
	if p.note != "" {
		b.WriteString(" · " + p.note)
	}
	return b.String()
}

// hoverJSON encodes the readout for the data attribute. Marshalling a
// slice of plain strings and floats cannot fail, so the error is dropped
// rather than threaded through every graph.
func hoverJSON(hs []hpoint) string {
	if len(hs) == 0 {
		return ""
	}
	b, _ := json.Marshal(hs)
	return string(b)
}

// segments splits points into runs the line may connect. A break is a
// skipped point or a delta well past the series' usual spacing — measured
// from the data rather than assumed from the tier, so a host reporting on a
// slower interval does not read as all gap.
func segments(points []gpoint) [][]gpoint {
	kept := slices.DeleteFunc(slices.Clone(points), func(p gpoint) bool { return p.skip })
	if len(kept) == 0 {
		return nil
	}
	gap := gapThreshold(kept)

	var out [][]gpoint
	cur := []gpoint{kept[0]}
	for _, p := range kept[1:] {
		if p.t.Sub(cur[len(cur)-1].t) > gap {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, p)
	}
	return append(out, cur)
}

// gapThreshold is 2.5 times the median spacing: tolerant of jitter, broken
// by anything resembling a missed report.
func gapThreshold(points []gpoint) time.Duration {
	if len(points) < 2 {
		return time.Duration(math.MaxInt64)
	}
	deltas := make([]time.Duration, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		deltas = append(deltas, points[i].t.Sub(points[i-1].t))
	}
	slices.Sort(deltas)
	return deltas[len(deltas)/2] * 5 / 2
}

// bandPath draws the min–max envelope: along the maxima, back along the
// minima, one closed subpath per segment. Single points get no band.
func bandPath(segs [][]gpoint, x func(time.Time) float64, y func(float64) float64) string {
	var d strings.Builder
	for _, seg := range segs {
		if len(seg) < 2 {
			continue
		}
		for i, p := range seg {
			if i == 0 {
				fmt.Fprintf(&d, "M%.1f %.1f", x(p.t), y(p.max))
			} else {
				fmt.Fprintf(&d, "L%.1f %.1f", x(p.t), y(p.max))
			}
		}
		for i := len(seg) - 1; i >= 0; i-- {
			fmt.Fprintf(&d, "L%.1f %.1f", x(seg[i].t), y(seg[i].min))
		}
		d.WriteString("Z")
	}
	return d.String()
}

// thin merges runs of points until at most target remain, aggregating the
// way the roll-up does: min of mins, max of maxes, sample-weighted mean. A
// bucket that is all absence stays absent.
func thin(points []gpoint, target int) []gpoint {
	if len(points) <= target {
		return points
	}
	k := (len(points) + target - 1) / target
	out := make([]gpoint, 0, target)
	for i := 0; i < len(points); i += k {
		bucket := points[i:min(i+k, len(points))]
		m := gpoint{t: bucket[0].t, skip: true, min: math.Inf(1)}
		var sum float64
		for _, p := range bucket {
			if p.skip {
				continue
			}
			w := max(p.weight, 1)
			m.skip = false
			m.min = min(m.min, p.min)
			if p.max >= m.max || m.note == "" {
				m.note = p.note // the peak's note, for a capacity readout
			}
			m.max = max(m.max, p.max)
			sum += p.mean * float64(w)
			m.weight += w
		}
		if m.skip {
			m.min = 0
		} else {
			m.mean = sum / float64(m.weight)
		}
		out = append(out, m)
	}
	return out
}

// niceCeil rounds up to 1, 2, or 5 times a power of ten, so the axis top
// is a number a person would pick.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 0
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

// xTicks labels the time axis at four even positions, in a form sized to
// the range.
func xTicks(from, to time.Time, x func(time.Time) float64) []tick {
	span := to.Sub(from)
	layout := "15:04"
	if span > 7*24*time.Hour {
		layout = "Jan 2"
	} else if span > 24*time.Hour {
		layout = "Mon 15:04"
	}
	var out []tick
	for i := 0; i <= 3; i++ {
		t := from.Add(span * time.Duration(i) / 3)
		out = append(out, tick{X: x(t), Label: t.Local().Format(layout)})
	}
	return out
}
