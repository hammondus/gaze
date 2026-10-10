package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

// newDirFixture opens a store with one enrolled host and a directive
// builder whose release lookup is stubbed.
func newDirFixture(t *testing.T, latest string) (*store.Store, *directives, int64, string) {
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

	d := newDirectives(s, latest) // the server is at the latest release
	d.gate.lookup = func() (string, error) { return latest, nil }
	return s, d, id, path
}

// echo simulates the agent's next report: version, generation, declined.
func echo(t *testing.T, s *store.Store, id int64, version string, gen int, declined string) {
	t.Helper()
	r := report.Report{
		Schema: report.Schema, Version: version, Generation: gen, Declined: declined,
		Host:  report.Host{Hostname: "web-01"},
		Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
	}
	if _, err := s.InsertReports(context.Background(), id, []report.Report{r}); err != nil {
		t.Fatal(err)
	}
}

// backdate moves the host's update request past the whole stagger window,
// so a test is not hostage to the host id's slot.
func backdate(t *testing.T, path string, id int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE hosts SET update_requested_at = ? WHERE id = ?`,
		time.Now().Add(-staggerWindow-time.Minute).Unix(), id); err != nil {
		t.Fatal(err)
	}
}

// TestDirectiveLifecycle is the stage's done-when, first half: a change is
// visible as sent until the agent echoes it, then as applied — and the
// server stops saying it.
func TestDirectiveLifecycle(t *testing.T) {
	s, d, id, _ := newDirFixture(t, "v1.0.0")
	ctx := context.Background()

	// No configuration set: the steady state says nothing.
	if dir, err := d.For(ctx, id); err != nil || dir != nil {
		t.Fatalf("directive with nothing to say = %v, %v", dir, err)
	}

	on := true
	gen, err := s.SetHostConfig(ctx, id, 5, 30, &on)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("first generation = %d, want 1", gen)
	}

	dir, err := d.For(ctx, id)
	if err != nil || dir == nil {
		t.Fatalf("directive = %v, %v", dir, err)
	}
	if dir.Generation != 1 || dir.SampleSeconds != 5 || dir.ReportSeconds != 30 ||
		dir.Containers == nil || !*dir.Containers {
		t.Fatalf("directive = %+v", dir)
	}

	// The agent applies and echoes: the server has nothing further to say.
	echo(t, s, id, "v1.0.0", 1, "")
	if dir, _ := d.For(ctx, id); dir != nil {
		t.Fatalf("directive re-sent after the echo: %+v", dir)
	}

	// A second change bumps the generation and starts again.
	if gen, _ := s.SetHostConfig(ctx, id, 10, 0, nil); gen != 2 {
		t.Fatalf("second generation = %d, want 2", gen)
	}
	if dir, _ := d.For(ctx, id); dir == nil || dir.Generation != 2 {
		t.Fatalf("second directive = %+v", dir)
	}
}

// TestDeclinedIsStored: the agent's refusal lands on the host row, which
// is what the fleet list renders — the done-when's second half.
func TestDeclinedIsStored(t *testing.T) {
	s, _, id, _ := newDirFixture(t, "v1.0.0")
	echo(t, s, id, "v1.0.0", 0, "configuration generation 1 refused: started without -allow-remote-config")

	cfg, err := s.HostConfig(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Declined == "" {
		t.Fatal("the refusal did not reach the host row")
	}
}

// TestUpdateGating covers the update trigger's three gates: the server's
// own version, the stagger slot, and the caught-up clearance.
func TestUpdateGating(t *testing.T) {
	s, d, id, path := newDirFixture(t, "v1.0.0")
	ctx := context.Background()
	echo(t, s, id, "v0.9.0", 0, "") // the agent is a release behind

	if err := s.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}

	// Freshly requested: the host's stagger slot may not have come up, so
	// nothing is asserted yet — backdating past the window forces it due.
	backdate(t, path, id)
	dir, err := d.For(ctx, id)
	if err != nil || dir == nil || !dir.Update {
		t.Fatalf("due update not sent: %v, %v", dir, err)
	}

	// An update-only directive carries the agent's own generation, so its
	// configuration half is a no-op.
	if dir.Generation != 0 || dir.SampleSeconds != 0 {
		t.Fatalf("update directive smuggled configuration: %+v", dir)
	}

	// The agent comes back on the new version: the request is spent.
	echo(t, s, id, "v1.0.0", 0, "")
	if dir, _ := d.For(ctx, id); dir != nil {
		t.Fatalf("update still offered after catch-up: %+v", dir)
	}
	cfg, _ := s.HostConfig(ctx, id)
	if !cfg.UpdateAsked.IsZero() {
		t.Fatal("update request not cleared after catch-up")
	}
}

// TestUpdateWithheldWhileServerIsBehind: a server that is not itself the
// latest release must never push its agents past its own schema.
func TestUpdateWithheldWhileServerIsBehind(t *testing.T) {
	s, d, id, path := newDirFixture(t, "v0.9.0") // server behind...
	d.gate.lookup = func() (string, error) { return "v1.0.0", nil }
	ctx := context.Background()
	echo(t, s, id, "v0.9.0", 0, "")

	if err := s.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}
	backdate(t, path, id)
	if dir, _ := d.For(ctx, id); dir != nil && dir.Update {
		t.Fatal("a behind server sent an update directive")
	}
}

// TestUpdateWithheldWhenLookupFails: no answer from the release page means
// no updates, and the failure is not retried per-report — the gate caches
// for an hour either way.
func TestUpdateWithheldWhenLookupFails(t *testing.T) {
	s, d, id, path := newDirFixture(t, "v1.0.0")
	calls := 0
	d.gate.lookup = func() (string, error) { calls++; return "", errNoNetwork }
	ctx := context.Background()
	echo(t, s, id, "v0.9.0", 0, "")

	if err := s.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}
	backdate(t, path, id)
	for range 5 {
		if dir, _ := d.For(ctx, id); dir != nil && dir.Update {
			t.Fatal("update sent with the release page unreachable")
		}
	}
	if calls != 1 {
		t.Fatalf("lookup called %d times, want 1 — GitHub is not a retry loop", calls)
	}
}

// TestUpdateSlotStable: the stagger slot is derived, not drawn, so a
// restarted server does not reshuffle the fleet's fetch times.
func TestUpdateSlotStable(t *testing.T) {
	if updateSlot(7) != updateSlot(7) {
		t.Fatal("a host's slot moved")
	}
	if updateSlot(7) >= staggerWindow {
		t.Fatal("slot outside the window")
	}
}

// TestUpdateProgressRecorded: the first send is recorded and kept across
// re-sends, the agent's failure text is stored from its report, and a new
// request starts the progress over.
func TestUpdateProgressRecorded(t *testing.T) {
	s, d, id, path := newDirFixture(t, "v1.0.0")
	ctx := context.Background()
	echo(t, s, id, "v0.9.0", 0, "")
	if err := s.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}
	cfg, _ := s.HostConfig(ctx, id)
	if !cfg.UpdateSent.IsZero() {
		t.Fatal("request marked sent before any reply carried it")
	}

	backdate(t, path, id)
	if dir, _ := d.For(ctx, id); dir == nil || !dir.Update {
		t.Fatal("due update not sent")
	}
	cfg, _ = s.HostConfig(ctx, id)
	first := cfg.UpdateSent
	if first.IsZero() {
		t.Fatal("send not recorded")
	}

	// The agent reports back, still old, with its reason; the server
	// re-sends, and the first send time stands.
	r := report.Report{
		Schema: report.Schema, Version: "v0.9.0", UpdateError: "cannot write to /usr/local/bin: read-only file system",
		Host:  report.Host{Hostname: "web-01"},
		Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
	}
	if _, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE hosts SET update_sent_at = update_sent_at - 120 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	d.For(ctx, id)
	cfg, _ = s.HostConfig(ctx, id)
	if !cfg.UpdateSent.Equal(first.Add(-120 * time.Second)) {
		t.Fatalf("a re-send moved the first send time: %v, want %v", cfg.UpdateSent, first.Add(-120*time.Second))
	}
	if cfg.UpdateError != r.UpdateError {
		t.Fatalf("UpdateError = %q, want the agent's text", cfg.UpdateError)
	}

	// Asking again starts the progress over.
	if err := s.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = s.HostConfig(ctx, id); !cfg.UpdateSent.IsZero() {
		t.Fatal("a new request kept the old send time")
	}
}

// TestRequestUpdateAllSkipsCurrent: the fleet button asks only the agents
// that need it — not one already on the latest release, and not a host
// that has never reported, which has no agent to tell.
func TestRequestUpdateAllSkipsCurrent(t *testing.T) {
	s, _, behind, _ := newDirFixture(t, "v1.0.0")
	ctx := context.Background()
	echo(t, s, behind, "v0.9.0", 0, "")

	token, err := s.Enroll(ctx, "current-01")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	echo(t, s, current, "v1.0.0", 0, "")
	if _, err := s.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}

	n, err := s.RequestUpdateAll(ctx, "v1.0.0")
	if err != nil || n != 1 {
		t.Fatalf("asked %d, %v; want 1", n, err)
	}
	if cfg, _ := s.HostConfig(ctx, current); !cfg.UpdateAsked.IsZero() {
		t.Error("a current agent was asked to update")
	}

	// Latest unknown: nobody is known to be current, so every reporting
	// host is asked.
	if n, err := s.RequestUpdateAll(ctx, ""); err != nil || n != 2 {
		t.Fatalf("latest unknown: asked %d, %v; want 2", n, err)
	}
}
