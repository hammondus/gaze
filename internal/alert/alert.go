// Package alert evaluates stored reports against threshold rules and mails
// on state transitions. It has two evaluation paths on purpose — thresholds
// run on ingest while the report is in memory, and staleness runs on a
// timer, because "a report did not arrive" is an event nothing delivers —
// see "Alerting has two evaluation paths, and one of them is a timer" in
// DESIGN-DECISIONS.md.
//
// The rules are code, not rows. Nothing in the roadmap gives rules an
// editing surface, and a table without an editor is a configuration file
// with extra steps; what persists is only each rule's standing per host, so
// a restart neither re-fires every open alert nor forgets one.
package alert

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/mailer"
)

// StaleAfter is how long after its last report a host counts as stale. The
// front ends colour on the same constant, so the fleet list and the alert
// mail can never disagree about what "stale" means.
const StaleAfter = 5 * time.Minute

// renotifyEvery is the re-notify suppression window: at most one message
// per rule per host per this interval, transitions included. It is what
// keeps an hour of flapping either side of a threshold at one message, and
// a long outage out of the mailbox.
const renotifyEvery = time.Hour

// liveWindow is how recent a report must be to be evaluated. A backlog
// flushed after an outage describes the past; alerting on it would page
// about history that has already resolved itself.
const liveWindow = 10 * time.Minute

// Observation is one value a rule extracted from a report: the whole host,
// or one instance of a fan-out rule (a mount path).
type Observation struct {
	Instance string
	Value    float64 // percent
}

// Rule is one threshold: fire when Value compares above Threshold for at
// least For. Values are percentages throughout — every default rule is a
// share of a capacity, which is the form a threshold is honest in.
type Rule struct {
	ID        string
	What      string // the phrase mail uses: "cpu", "filesystem /data"
	Threshold float64
	For       time.Duration
	observe   func(r *report.Report) []Observation
}

// rules is the default set. Thresholds follow the TUI's critical colours;
// the durations are what make them alerts rather than noise — "above 90
// for fifteen minutes" is a different statement from "above 90".
var rules = []Rule{
	{
		ID: "cpu", What: "cpu", Threshold: 90, For: 15 * time.Minute,
		observe: func(r *report.Report) []Observation {
			return []Observation{{Value: r.CPU.Mean}}
		},
	},
	{
		ID: "memory", What: "memory", Threshold: 92, For: 15 * time.Minute,
		observe: func(r *report.Report) []Observation {
			if r.Memory.Total == 0 {
				return nil
			}
			return []Observation{{Value: r.Memory.Used.Mean / float64(r.Memory.Total) * 100}}
		},
	},
	{
		// Swap is judged harder than memory, same as the TUI's colours: a
		// machine using most of its swap is already paying for it.
		ID: "swap", What: "swap", Threshold: 80, For: 15 * time.Minute,
		observe: func(r *report.Report) []Observation {
			if r.Swap.Total == 0 {
				return nil // no swap is not full swap
			}
			return []Observation{{Value: r.Swap.Used.Mean / float64(r.Swap.Total) * 100}}
		},
	},
	{
		ID: "mount", What: "filesystem", Threshold: 90, For: 15 * time.Minute,
		observe: func(r *report.Report) []Observation {
			out := make([]Observation, 0, len(r.Mounts))
			for _, m := range r.Mounts {
				out = append(out, Observation{Instance: m.Path, Value: m.Percent})
			}
			return out
		},
	},
}

// staleRule is the name the staleness path files its state under. It is
// not in rules: nothing in a report can observe a report's absence.
const staleRule = "stale"

// Alerter owns the state machines and the mail. Send errors are logged and
// the transition is still recorded; mailed_at stays unset, so the next
// transition tries again.
type Alerter struct {
	store  *store.Store
	sender mailer.Sender
	to     []string

	// now is the server clock, replaceable in tests. Breach durations are
	// measured on the sample clock; suppression and staleness on this one.
	now func() time.Time
}

// New wires the alerter. The sender decides where mail really goes — SMTP,
// the log, or a test's memory.
func New(s *store.Store, sender mailer.Sender, to []string) *Alerter {
	return &Alerter{store: s, sender: sender, to: to, now: time.Now}
}

// Evaluate runs the threshold rules against one stored report. Errors are
// the caller's to log; evaluation must never fail an ingest.
func (a *Alerter) Evaluate(ctx context.Context, hostID int64, r *report.Report) error {
	now := a.now()
	if now.Sub(r.End) > liveWindow {
		return nil // backlog: it describes the past
	}

	states, err := a.store.AlertStates(ctx, hostID)
	if err != nil {
		return err
	}

	host := r.Host.Hostname
	for _, rule := range rules {
		seen := map[string]bool{}
		for _, o := range rule.observe(r) {
			seen[o.Instance] = true
			st, ok := states[[2]string{rule.ID, o.Instance}]
			if !ok {
				st = store.AlertState{HostID: hostID, Rule: rule.ID, Instance: o.Instance}
			}
			if err := a.step(ctx, host, rule, o, st, r.End, now); err != nil {
				return err
			}
		}
		// A state row whose subject the report no longer carries — an
		// unmounted filesystem — has nothing left to be true or false
		// about. The row is dropped without a recovery message: "resolved"
		// and "gone" are different facts, and mailing one as the other
		// would say the disk freed space it did not.
		for key, st := range states {
			if key[0] == rule.ID && st.State != store.AlertOK && !seen[key[1]] {
				if err := a.store.DeleteAlertState(ctx, hostID, key[0], key[1]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// step advances one rule's state machine by one observation. sample is the
// report's end on the agent's clock, which is what breach durations are
// measured on; now is the server clock, which suppression reads.
func (a *Alerter) step(ctx context.Context, host string, rule Rule, o Observation, st store.AlertState, sample, now time.Time) error {
	breach := o.Value > rule.Threshold

	switch {
	case st.State == store.AlertOK && breach:
		st.State, st.Since, st.Changed = store.AlertPending, sample, now
		if rule.For == 0 {
			st.State = store.AlertFiring
			a.notify(ctx, &st, now, a.fireMessage(host, rule, o, sample))
		}

	case st.State == store.AlertPending && breach:
		if sample.Sub(st.Since) < rule.For {
			return nil // still pending; nothing changed
		}
		st.State, st.Changed = store.AlertFiring, now
		a.notify(ctx, &st, now, a.fireMessage(host, rule, o, st.Since))

	case st.State == store.AlertPending && !breach:
		st.State, st.Changed = store.AlertOK, now

	case st.State == store.AlertFiring && !breach:
		st.State, st.Changed = store.AlertOK, now
		a.notify(ctx, &st, now, a.recoverMessage(host, rule, o))

	default:
		return nil // OK and quiet, or firing and still firing
	}
	return a.store.SetAlertState(ctx, st)
}

// SweepStaleness fires for hosts the server has stopped hearing from. It
// runs on a timer because nothing else would run it: a missing report
// triggers no ingest. Receive time on purpose — a host with a broken clock
// must still go stale. A host that has never reported is skipped: that is
// a setup in progress, not an outage.
func (a *Alerter) SweepStaleness(ctx context.Context) error {
	hbs, err := a.store.Heartbeats(ctx)
	if err != nil {
		return err
	}
	now := a.now()

	for _, hb := range hbs {
		if hb.LastSeen.IsZero() {
			continue
		}
		states, err := a.store.AlertStates(ctx, hb.HostID)
		if err != nil {
			return err
		}
		st, ok := states[[2]string{staleRule, ""}]
		if !ok {
			st = store.AlertState{HostID: hb.HostID, Rule: staleRule}
		}
		stale := now.Sub(hb.LastSeen) > StaleAfter

		switch {
		case st.State != store.AlertFiring && stale:
			// The gap is its own duration; there is no pending step to
			// wait out on top of it.
			st.State, st.Since, st.Changed = store.AlertFiring, hb.LastSeen, now
			a.notify(ctx, &st, now, &mailer.Message{
				To:      a.to,
				Subject: fmt.Sprintf("[gaze] %s: no reports for %s", hb.Name, ago(now.Sub(hb.LastSeen))),
				Text: fmt.Sprintf("Host %s has stopped reporting.\n\nLast report received %s (%s ago).\n",
					hb.Name, hb.LastSeen.Format(time.RFC3339), ago(now.Sub(hb.LastSeen))),
			})

		case st.State == store.AlertFiring && !stale:
			st.State, st.Changed = store.AlertOK, now
			a.notify(ctx, &st, now, &mailer.Message{
				To:      a.to,
				Subject: fmt.Sprintf("[gaze] %s: reporting again", hb.Name),
				Text: fmt.Sprintf("Host %s is reporting again after a gap; silent since %s.\n",
					hb.Name, st.Since.Format(time.RFC3339)),
			})

		default:
			continue // no transition, nothing to write
		}
		if err := a.store.SetAlertState(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// notify sends unless a message for this rule and host went out inside the
// suppression window. A suppressed or failed send leaves mailed_at alone,
// and the transition is recorded either way: the state must stay true even
// when the mailbox is spared.
func (a *Alerter) notify(ctx context.Context, st *store.AlertState, now time.Time, m *mailer.Message) {
	if !st.Mailed.IsZero() && now.Sub(st.Mailed) < renotifyEvery {
		return
	}
	if err := a.sender.Send(ctx, m); err != nil {
		log.Printf("alert: send %q: %v", m.Subject, err)
		return
	}
	st.Mailed = now
}

func (a *Alerter) fireMessage(host string, rule Rule, o Observation, since time.Time) *mailer.Message {
	what := rule.What
	if o.Instance != "" {
		what += " " + o.Instance
	}
	return &mailer.Message{
		To:      a.to,
		Subject: fmt.Sprintf("[gaze] %s: %s at %.0f%%", host, what, o.Value),
		Text: fmt.Sprintf("Host %s: %s is at %.0f%%, above %.0f%% since %s.\n",
			host, what, o.Value, rule.Threshold, since.Format(time.RFC3339)),
	}
}

func (a *Alerter) recoverMessage(host string, rule Rule, o Observation) *mailer.Message {
	what := rule.What
	if o.Instance != "" {
		what += " " + o.Instance
	}
	return &mailer.Message{
		To:      a.to,
		Subject: fmt.Sprintf("[gaze] %s: %s recovered (%.0f%%)", host, what, o.Value),
		Text: fmt.Sprintf("Host %s: %s is back under its %.0f%% threshold, at %.0f%%.\n",
			host, what, rule.Threshold, o.Value),
	}
}

// ago renders a duration the way a subject line wants it: coarse.
func ago(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "1 minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// Async wraps a sender so Send returns at once and delivery happens in the
// background. Evaluation runs inside an agent's POST, and a wedged SMTP
// server must slow mail, not monitoring. Transitions are rare and
// suppression bounds them, so unbounded goroutines here are a herd of at
// most a few. The trade is stated plainly: a background delivery that
// fails is logged but still counts as sent for suppression, which errs
// toward a quiet mailbox rather than a retry storm.
func Async(s mailer.Sender) mailer.Sender {
	return asyncSender{s}
}

type asyncSender struct{ inner mailer.Sender }

func (s asyncSender) Send(_ context.Context, m *mailer.Message) error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.inner.Send(ctx, m); err != nil {
			log.Printf("alert: send %q: %v", m.Subject, err)
		}
	}()
	return nil
}

// Recipients parses a comma-separated address list from the environment
// form GAZE_ALERT_TO uses.
func Recipients(env string) []string {
	var out []string
	for part := range strings.SplitSeq(env, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
