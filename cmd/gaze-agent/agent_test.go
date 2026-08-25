package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hammondus/gaze/internal/metrics"
	"github.com/hammondus/gaze/internal/report"
)

func testAgent(t *testing.T, server string, allowRemote bool) *agent {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	return &agent{
		client:            newClient(u, writeToken(t, 0o600), "test"),
		version:           "test",
		hostID:            "test-host",
		allowRemoteConfig: allowRemote,
		// An empty tree: every collector read fails, which is exactly the
		// "errors do not clear the screen" path — snapshots still arrive and
		// reports still flow.
		newCollector: func(disableContainers bool) *metrics.Collector {
			return metrics.NewWithSource(metrics.Source{
				Proc: fstest.MapFS{}, Sys: fstest.MapFS{},
			}, metrics.Options{DisableContainers: disableContainers})
		},
		cfg:  config{sample: 10 * time.Millisecond, report: 40 * time.Millisecond},
		wake: make(chan struct{}, 1),
	}
}

// TestApplyRefusedWithoutFlag pins the default: a directive cannot
// reconfigure an agent that was not started with -allow-remote-config, and
// the generation is not echoed as applied.
func TestApplyRefusedWithoutFlag(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", false)
	before := a.cfg
	on := true
	a.apply(&report.Directive{Generation: 3, SampleSeconds: 1, ReportSeconds: 5, Containers: &on})
	if a.cfg != before {
		t.Errorf("config changed without the flag: %+v", a.cfg)
	}
}

// TestApplyDirective covers the allowed path: intervals applied with their
// clamps, the container toggle, and the generation recorded for the echo.
func TestApplyDirective(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", true)
	off := false
	a.apply(&report.Directive{Generation: 4, SampleSeconds: 2, ReportSeconds: 1, Containers: &off})
	if a.cfg.generation != 4 {
		t.Errorf("generation = %d, want 4", a.cfg.generation)
	}
	if a.cfg.sample != 2*time.Second {
		t.Errorf("sample = %s, want 2s", a.cfg.sample)
	}
	if a.cfg.report != 2*time.Second {
		t.Errorf("report = %s, want clamped up to the sample interval", a.cfg.report)
	}
	if !a.cfg.containersOff {
		t.Error("containers still on after the directive turned them off")
	}
	// The same generation again is a no-op, not a re-application.
	a.apply(&report.Directive{Generation: 4, SampleSeconds: 9})
	if a.cfg.sample != 2*time.Second {
		t.Error("a repeated generation was re-applied")
	}
}

// TestEmit covers the window-to-ring hand-off and the generation echo.
func TestEmit(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", false)
	a.cfg.generation = 6
	a.emit() // empty window: nothing to say
	if a.pending() != 0 {
		t.Fatal("an empty window produced a report")
	}
	a.window = []metrics.Snapshot{{Taken: time.Now()}}
	a.emit()
	if a.pending() != 1 {
		t.Fatal("no report queued")
	}
	if got := a.take(1)[0]; got.Generation != 6 || got.Version != "test" {
		t.Errorf("report = gen %d version %q, want gen 6 version test", got.Generation, got.Version)
	}
}

// receiver is a restartable ingest endpoint: an httptest.Server cannot come
// back on the same port, and the outage test needs exactly that.
type receiver struct {
	t    *testing.T
	addr string
	srv  *http.Server

	mu      sync.Mutex
	reports []report.Report
}

func (rc *receiver) handle(w http.ResponseWriter, r *http.Request) {
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		rc.t.Error(err)
		return
	}
	var batch []report.Report
	if err := json.NewDecoder(zr).Decode(&batch); err != nil {
		rc.t.Error(err)
		return
	}
	rc.mu.Lock()
	rc.reports = append(rc.reports, batch...)
	rc.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (rc *receiver) start() {
	var l net.Listener
	var err error
	// The port was just released by stop; rebinding can need a moment.
	for range 50 {
		if l, err = net.Listen("tcp", rc.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		rc.t.Fatalf("cannot rebind %s: %v", rc.addr, err)
	}
	rc.addr = l.Addr().String()
	rc.srv = &http.Server{Handler: http.HandlerFunc(rc.handle)}
	go rc.srv.Serve(l)
}

func (rc *receiver) stop()      { rc.srv.Close() }
func (rc *receiver) count() int { rc.mu.Lock(); defer rc.mu.Unlock(); return len(rc.reports) }

// TestGapIsFilledAfterOutage is the stage 3 done-when condition, compressed:
// the server goes away, the agent keeps sampling and queueing, and when the
// server returns the backlog arrives — covering the outage, in order.
func TestGapIsFilledAfterOutage(t *testing.T) {
	rc := &receiver{t: t, addr: "127.0.0.1:0"}
	rc.start()

	a := testAgent(t, "http://"+rc.addr, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.run(ctx); close(done) }()

	waitFor := func(what string, deadline time.Duration, cond func() bool) {
		t.Helper()
		for start := time.Now(); time.Since(start) < deadline; time.Sleep(10 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	waitFor("first reports", 5*time.Second, func() bool { return rc.count() >= 2 })

	outageStart := time.Now()
	rc.stop()
	waitFor("a backlog to build", 5*time.Second, func() bool { return a.pending() >= 3 })
	outageEnd := time.Now()
	rc.start()

	// The flusher is asleep in a jittered backoff; give it room to wake.
	waitFor("the backlog to drain", 20*time.Second, func() bool { return a.pending() == 0 && rc.count() >= 5 })

	cancel()
	<-done

	rc.mu.Lock()
	defer rc.mu.Unlock()
	covered := false
	for i, r := range rc.reports {
		if i > 0 && r.Start.Before(rc.reports[i-1].Start) {
			t.Errorf("report %d arrived out of order: %v after %v", i, r.Start, rc.reports[i-1].Start)
		}
		if !r.Start.Before(outageStart) && r.Start.Before(outageEnd) {
			covered = true
		}
	}
	if !covered {
		t.Errorf("no received report covers the outage %v..%v: the gap was not filled", outageStart, outageEnd)
	}
}

// TestDeclinedEcho pins the stage-8 contract: a refusal rides the next
// report, and clears once the server stops asking for the refused thing.
func TestDeclinedEcho(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", false)

	a.apply(&report.Directive{Generation: 3, SampleSeconds: 30})
	a.window = []metrics.Snapshot{{Taken: time.Now()}}
	a.emit()
	got := a.take(1)[0]
	if got.Declined == "" || got.Generation != 0 {
		t.Fatalf("refused directive not echoed: declined=%q gen=%d", got.Declined, got.Generation)
	}
	a.drop(1)

	// The server gives up (sends the agent's own generation): nothing
	// stands refused any more.
	a.apply(&report.Directive{Generation: 0})
	a.window = []metrics.Snapshot{{Taken: time.Now()}}
	a.emit()
	if got := a.take(1)[0]; got.Declined != "" {
		t.Fatalf("declined did not clear: %q", got.Declined)
	}
}

// TestUpdateRefusedWithoutFlag: the update trigger without
// -allow-remote-update is declined with why, and nothing runs.
func TestUpdateRefusedWithoutFlag(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", false)
	ran := false
	a.selfUpdate = func() error { ran = true; return nil }

	a.apply(&report.Directive{Update: true})
	if ran {
		t.Fatal("self-update ran without -allow-remote-update")
	}
	a.mu.Lock()
	declined := a.declined
	a.mu.Unlock()
	if !strings.Contains(declined, "allow-remote-update") {
		t.Fatalf("declined = %q, want the flag named", declined)
	}
}

// TestUpdateRunsOncePerHour: the server re-sends the trigger every report
// until the version changes, and each attempt is a GitHub download, so
// attempts are gated to one an hour.
func TestUpdateRunsOncePerHour(t *testing.T) {
	a := testAgent(t, "http://127.0.0.1:1", false)
	a.allowRemoteUpdate = true

	var mu sync.Mutex
	runs := 0
	done := make(chan struct{}, 4)
	a.selfUpdate = func() error {
		mu.Lock()
		runs++
		mu.Unlock()
		done <- struct{}{}
		return nil
	}

	a.apply(&report.Directive{Update: true})
	<-done
	a.apply(&report.Directive{Update: true})
	a.apply(&report.Directive{Update: true})

	mu.Lock()
	got := runs
	mu.Unlock()
	if got != 1 {
		t.Fatalf("self-update ran %d times inside the hour, want 1", got)
	}
	a.mu.Lock()
	declined := a.declined
	a.mu.Unlock()
	if declined != "" {
		t.Fatalf("a gated retry read as a refusal: %q", declined)
	}

	// Past the gate, the next trigger runs again.
	a.mu.Lock()
	a.updateStarted = time.Now().Add(-2 * time.Hour)
	a.mu.Unlock()
	a.apply(&report.Directive{Update: true})
	<-done
	mu.Lock()
	got = runs
	mu.Unlock()
	if got != 2 {
		t.Fatalf("self-update did not run after the gate expired: %d", got)
	}
}
