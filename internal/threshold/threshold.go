// Package threshold is the one table of warning and critical points that
// every surface judges a percentage by: the terminal's colours, the web
// pages' colours, and the alert rules. Critical is the alert threshold, so
// red anywhere means the same thing — at or past the point that mails you.
//
// Standard library only: the gaze binary imports it, and its dependency
// footprint is a stated contract.
package threshold

// T is the warning and critical points for one kind of metric, as
// percentages.
type T struct{ Warn, Crit float64 }

// Level is how a value stands against its thresholds.
type Level int

const (
	OK Level = iota
	Warn
	Crit
)

// Of reports where v stands against t.
func (t T) Of(v float64) Level {
	switch {
	case v >= t.Crit:
		return Crit
	case v >= t.Warn:
		return Warn
	default:
		return OK
	}
}

var (
	CPU    = T{Warn: 70, Crit: 90}
	Memory = T{Warn: 80, Crit: 92}
	// Swap warns far earlier than memory. A machine using its swap at all is
	// already paying for it, whereas full memory is what memory is for.
	Swap = T{Warn: 25, Crit: 80}
	Disk = T{Warn: 80, Crit: 90}
)
