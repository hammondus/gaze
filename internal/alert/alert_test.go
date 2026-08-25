package alert

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/mailer"
	_ "modernc.org/sqlite"
)

var base = time.Now().Truncate(time.Minute)

type fixture struct {
	t      *testing.T
	store  *store.Store
	sent   *mailer.MemorySender
	a      *Alerter
	hostID int64
	dbPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gaze.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	token, err := s.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}

	sent := &mailer.MemorySender{}
	a := New(s, sent, []string{"ops@example.com"})
	a.now = func() time.Time { return base }
	return &fixture{t: t, store: s, sent: sent, a: a, hostID: id, dbPath: path}
}

// at moves the alerter's clock to base+d.
func (f *fixture) at(d time.Duration) time.Time {
	now := base.Add(d)
	f.a.now = func() time.Time { return now }
	return now
}

// rpt builds a minimal live report ending at base+d.
func rpt(d time.Duration, cpu float64) report.Report {
	end := base.Add(d)
	return report.Report{
		Schema: report.Schema,
		Host:   report.Host{Hostname: "web-01"},
		Start:  end.Add(-time.Minute), End: end, Samples: 6,
		CPU:    report.Stat{Min: cpu - 2, Max: cpu + 2, Mean: cpu},
		Memory: report.Gauge{Total: 8 << 30, Used: report.Stat{Mean: 2 << 30}},
	}
}

// eval moves the clock to the report's end and evaluates it, which is how
// ingest behaves: the report just arrived.
func (f *fixture) eval(r report.Report) {
	f.t.Helper()
	f.at(r.End.Sub(base))
	if err := f.a.Evaluate(context.Background(), f.hostID, &r); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) mails() []string {
	var out []string
	for _, m := range f.sent.Sent() {
		out = append(out, m.Subject)
	}
	return out
}

// setLastSeen backdates the host's receive time — test-only surgery the
// staleness sweep needs, since the store stamps inserts with its own clock.
func (f *fixture) setLastSeen(at time.Time) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.dbPath)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE hosts SET last_seen_at = ? WHERE id = ?`,
		at.Unix(), f.hostID); err != nil {
		f.t.Fatal(err)
	}
}

// TestThresholdLifecycle walks one rule through its whole state machine:
// pending is silent, firing mails once, a sustained breach stays quiet,
// and recovery past the suppression window mails once more.
func TestThresholdLifecycle(t *testing.T) {
	f := newFixture(t)

	f.eval(rpt(0, 95))
	f.eval(rpt(5*time.Minute, 95))
	if n := len(f.mails()); n != 0 {
		t.Fatalf("pending breach mailed: %v", f.mails())
	}

	f.eval(rpt(15*time.Minute, 95))
	mails := f.mails()
	if len(mails) != 1 || !strings.Contains(mails[0], "cpu at 95%") {
		t.Fatalf("firing = %v, want one cpu message", mails)
	}

	// Still breaching: firing already said so.
	f.eval(rpt(20*time.Minute, 97))
	f.eval(rpt(30*time.Minute, 99))
	if n := len(f.mails()); n != 1 {
		t.Fatalf("sustained breach re-mailed: %v", f.mails())
	}

	// Recovery, past the suppression window: one recovery message.
	f.eval(rpt(80*time.Minute, 20))
	mails = f.mails()
	if len(mails) != 2 || !strings.Contains(mails[1], "recovered") {
		t.Fatalf("recovery = %v", mails)
	}
}

// TestShortSpikeIsSilent: above the threshold, but not for the duration.
// "Above 90 for fifteen minutes" is the statement, not "above 90".
func TestShortSpikeIsSilent(t *testing.T) {
	f := newFixture(t)
	f.eval(rpt(0, 99))
	f.eval(rpt(5*time.Minute, 99))
	f.eval(rpt(10*time.Minute, 30)) // back down before 15m
	f.eval(rpt(25*time.Minute, 30))
	if n := len(f.mails()); n != 0 {
		t.Fatalf("a short spike mailed: %v", f.mails())
	}
}

// TestFlappingHourSendsOneMessage is the stage's done-when: a rule that
// flaps either side of its threshold for an hour sends one message.
func TestFlappingHourSendsOneMessage(t *testing.T) {
	f := newFixture(t)

	// Minute-by-minute reports for an hour: 20 minutes above, 5 below,
	// repeating. Each high phase is long enough to fire; without
	// suppression that is a message every cycle, in both directions.
	for m := 0; m <= 60; m++ {
		cpu := 95.0
		if m%25 >= 20 {
			cpu = 30
		}
		f.eval(rpt(time.Duration(m)*time.Minute, cpu))
	}
	if n := len(f.mails()); n != 1 {
		t.Fatalf("a flapping hour sent %d messages: %v", n, f.mails())
	}
}

// TestMountInstances: the mount rule fans out per path, and a vanished
// mount drops its state without a fake recovery.
func TestMountInstances(t *testing.T) {
	f := newFixture(t)

	withMounts := func(d time.Duration, dataPct float64, includeData bool) report.Report {
		r := rpt(d, 10)
		r.Mounts = []report.Mount{{Device: "/dev/sda1", Path: "/", FSType: "ext4", Total: 100, Used: 50, Percent: 50}}
		if includeData {
			r.Mounts = append(r.Mounts, report.Mount{Device: "/dev/sdb1", Path: "/data", FSType: "xfs", Total: 100, Used: uint64(dataPct), Percent: dataPct})
		}
		return r
	}

	f.eval(withMounts(0, 95, true))
	f.eval(withMounts(15*time.Minute, 95, true))
	mails := f.mails()
	if len(mails) != 1 || !strings.Contains(mails[0], "filesystem /data") {
		t.Fatalf("mount firing = %v, want one /data message", mails)
	}

	// The mount disappears while firing: the row goes, and nothing mails —
	// "resolved" and "gone" are different facts.
	f.eval(withMounts(20*time.Minute, 0, false))
	if n := len(f.mails()); n != 1 {
		t.Fatalf("an unmounted filesystem mailed: %v", f.mails())
	}
	states, err := f.store.AlertStates(context.Background(), f.hostID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states[[2]string{"mount", "/data"}]; ok {
		t.Fatal("state row for the vanished mount survived")
	}
}

// TestBacklogIsNotEvaluated: a flushed backlog describes the past, and
// paging about history that already resolved itself helps no one.
func TestBacklogIsNotEvaluated(t *testing.T) {
	f := newFixture(t)
	f.at(0)
	old := rpt(-time.Hour, 99)
	if err := f.a.Evaluate(context.Background(), f.hostID, &old); err != nil {
		t.Fatal(err)
	}
	states, err := f.store.AlertStates(context.Background(), f.hostID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 || len(f.mails()) != 0 {
		t.Fatalf("backlog evaluated: states=%v mails=%v", states, f.mails())
	}
}

// TestNoSwapIsNotFullSwap: a machine without swap must not read as one
// whose swap is exhausted.
func TestNoSwapIsNotFullSwap(t *testing.T) {
	f := newFixture(t)
	r := rpt(0, 10)
	r.Swap = report.Gauge{Total: 0, Used: report.Stat{Mean: 0}}
	f.eval(r)
	r = rpt(16*time.Minute, 10)
	r.Swap = report.Gauge{Total: 0}
	f.eval(r)
	if n := len(f.mails()); n != 0 {
		t.Fatalf("a swapless machine mailed: %v", f.mails())
	}
}

// TestStaleness covers the timer path: fire on silence, stay quiet while
// it lasts, and re-fire only past the suppression window.
func TestStaleness(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A second host that has never reported: setup in progress, not an
	// outage. It must never mail.
	if _, err := f.store.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}

	f.setLastSeen(base.Add(-10 * time.Minute))
	f.at(0)
	if err := f.a.SweepStaleness(ctx); err != nil {
		t.Fatal(err)
	}
	mails := f.mails()
	if len(mails) != 1 || !strings.Contains(mails[0], "no reports") {
		t.Fatalf("stale sweep = %v", mails)
	}

	// A long outage is not a mailbox: later sweeps see no transition.
	f.at(30 * time.Minute)
	if err := f.a.SweepStaleness(ctx); err != nil {
		t.Fatal(err)
	}
	f.at(3 * time.Hour)
	if err := f.a.SweepStaleness(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.mails()); n != 1 {
		t.Fatalf("a long outage re-mailed: %v", f.mails())
	}

	// Reports resume: the transition mails, the window having long passed.
	f.setLastSeen(base.Add(3*time.Hour + time.Minute))
	f.at(3*time.Hour + 2*time.Minute)
	if err := f.a.SweepStaleness(ctx); err != nil {
		t.Fatal(err)
	}
	mails = f.mails()
	if len(mails) != 2 || !strings.Contains(mails[1], "reporting again") {
		t.Fatalf("recovery sweep = %v", mails)
	}
}
