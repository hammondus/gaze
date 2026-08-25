package main

import "github.com/hammondus/gaze/internal/report"

// ringCap is about an hour of unsent reports at the default interval.
const ringCap = 60

// ring is a bounded first-in-first-out buffer of unsent reports. When full
// it drops the oldest: an unbounded buffer converts a long server outage
// into an out-of-memory kill on the host being watched, which is a
// monitoring system causing the incident.
//
// ring is not safe for concurrent use; the agent's mutex guards it.
type ring struct {
	buf []report.Report
}

func (r *ring) push(rep report.Report) {
	r.buf = append(r.buf, rep)
	if over := len(r.buf) - ringCap; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
}

// peek returns up to n of the oldest reports without removing them. The
// caller sends them, and drops them only once the server has them.
func (r *ring) peek(n int) []report.Report {
	n = min(n, len(r.buf))
	out := make([]report.Report, n)
	copy(out, r.buf[:n])
	return out
}

// drop removes the n oldest reports, copying rather than re-slicing so the
// backing array cannot grow without bound.
func (r *ring) drop(n int) {
	n = min(n, len(r.buf))
	r.buf = append(r.buf[:0], r.buf[n:]...)
}

func (r *ring) len() int { return len(r.buf) }
