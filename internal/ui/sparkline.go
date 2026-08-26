package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sparkChars run from a low bar to a full one. A space is used for zero, so a
// genuinely idle stretch reads as flat rather than as a low hum.
var sparkChars = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// ring is a fixed-size history of one metric, oldest to newest.
//
// This is the only state the display layer keeps. The collector deals in
// instants and holds no history, because a snapshot is what a snapshot means;
// how much of the past to draw is a display decision.
type ring struct {
	buf  []float64
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]float64, n)} }

// push records a new sample, discarding the oldest.
func (r *ring) push(v float64) {
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// values returns the history oldest first.
func (r *ring) values() []float64 {
	if !r.full {
		return r.buf[:r.next]
	}
	out := make([]float64, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}

// max returns the largest sample held.
func (r *ring) max() float64 {
	var m float64
	for _, v := range r.values() {
		if v > m {
			m = v
		}
	}
	return m
}

// spark renders the whole history as a sparkline scaled to peak.
//
// Pass a fixed peak for a metric with a natural ceiling, such as 100 for a
// percentage, so the line's height means the same thing from one refresh to
// the next. Pass zero for an open-ended metric such as a byte rate, and the
// line rescales to its own maximum.
func (r *ring) spark(width int, peak float64, style lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	vals := r.values()
	if len(vals) > width {
		// A history longer than the line is compressed, keeping each bucket's
		// maximum. These are gauges against a fixed ceiling, and a one-second
		// spike to that ceiling is exactly what the history exists to show;
		// averaging it into its bucket rounds it away.
		vals = maxBuckets(vals, width)
	}
	if peak <= 0 {
		peak = r.max()
	}

	var b strings.Builder
	// A history shorter than the line is right-aligned, so the newest sample
	// always sits at the right edge and the line grows leftwards as it fills.
	b.WriteString(strings.Repeat(" ", width-len(vals)))
	for _, v := range vals {
		switch {
		case peak <= 0 || v <= 0:
			b.WriteRune(' ')
		default:
			i := int(v / peak * float64(len(sparkChars)))
			if i >= len(sparkChars) {
				i = len(sparkChars) - 1
			}
			b.WriteRune(sparkChars[i])
		}
	}
	return style.Render(b.String())
}

// maxBuckets reduces vals to width samples, one per even slice of the input,
// each carrying its slice's largest value. Only called with len(vals) > width,
// so every slice is non-empty.
func maxBuckets(vals []float64, width int) []float64 {
	out := make([]float64, width)
	for i := range out {
		m := vals[i*len(vals)/width]
		for _, v := range vals[i*len(vals)/width : (i+1)*len(vals)/width] {
			if v > m {
				m = v
			}
		}
		out[i] = m
	}
	return out
}
