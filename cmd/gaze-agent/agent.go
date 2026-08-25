package main

import (
	"context"
	"errors"
	"hash/fnv"
	"log"
	"sync"
	"time"

	"github.com/hammondus/gaze/internal/metrics"
	"github.com/hammondus/gaze/internal/report"
)

// config is the agent's adjustable behaviour: the two intervals, whether
// container collection is off, and the generation of the directive that last
// changed any of it.
type config struct {
	sample        time.Duration
	report        time.Duration
	containersOff bool
	generation    int
}

// agent samples the machine on a short interval and posts reductions on a
// long one. Three goroutines share it: the sampler appends snapshots to the
// window, the reporter reduces the window into the ring on the report tick,
// and the flusher drains the ring to the server. The mutex guards the
// window, the ring, and the config; the collector itself is owned by the
// sampler alone, because it is not safe for concurrent use.
type agent struct {
	client            *client
	version           string
	hostID            string
	cmdlines          bool
	allowRemoteConfig bool

	// newCollector builds a collector, so the sampler can swap it out when a
	// directive toggles container collection, and tests can supply fixtures.
	newCollector func(disableContainers bool) *metrics.Collector

	mu     sync.Mutex
	cfg    config
	window []metrics.Snapshot
	ring   ring

	// lastRefused is the generation whose refusal was already logged, so a
	// steady-state server re-sending the same directive every minute does
	// not fill the journal with the same sentence.
	lastRefused int

	// wake nudges the flusher when a report lands in the ring. Buffer of
	// one: a nudge while it is already draining changes nothing.
	wake chan struct{}
}

// run starts the three loops and blocks until ctx is cancelled, then tries
// once, briefly, to flush what is left rather than losing the final window.
func (a *agent) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){a.sampleLoop, a.reportLoop, a.flushLoop} {
		wg.Go(func() { loop(ctx) })
	}
	wg.Wait()

	a.emit()
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		batch := a.take(postBatch)
		if len(batch) == 0 {
			return
		}
		if _, err := a.client.post(fctx, batch); err != nil {
			log.Printf("final flush: %v (%d reports unsent)", err, a.pending())
			return
		}
		a.drop(len(batch))
	}
}

// sampleLoop collects on the sample interval, chained rather than on a
// ticker for the same reason the TUI chains: a machine too loaded to keep up
// should sample less often, not queue work it never finishes.
func (a *agent) sampleLoop(ctx context.Context) {
	a.mu.Lock()
	containersOff := a.cfg.containersOff
	a.mu.Unlock()
	col := a.newCollector(containersOff)

	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s := col.Collect(cctx)
		cancel()

		a.mu.Lock()
		a.window = append(a.window, s)
		// The reporter drains the window every report interval; a cap twice
		// that size only matters if it has stalled, and bounds memory then.
		if limit := 2*int(a.cfg.report/a.cfg.sample) + 2; len(a.window) > limit {
			a.window = append(a.window[:0], a.window[len(a.window)-limit:]...)
		}
		interval := a.cfg.sample
		off := a.cfg.containersOff
		a.mu.Unlock()

		if off != containersOff {
			containersOff = off
			col = a.newCollector(off)
			log.Printf("container collection now %s", map[bool]string{true: "off", false: "on"}[off])
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// reportLoop reduces the window into the ring on every report tick. Ticks
// are aligned to the wall clock plus this host's offset, so a fleet
// installed by one script does not post in unison — see "Two herds, and they
// need different fixes" in DESIGN-DECISIONS.md.
func (a *agent) reportLoop(ctx context.Context) {
	for {
		a.mu.Lock()
		interval := a.cfg.report
		a.mu.Unlock()

		t := time.NewTimer(time.Until(nextTick(time.Now(), interval, a.offset(interval))))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		a.emit()
	}
}

// emit reduces the current window to one report and queues it for sending.
// An empty window — a collector that produced nothing at all — queues
// nothing: there is no observation to report, and a report of a gap is what
// the server's staleness sweep exists to notice.
func (a *agent) emit() {
	a.mu.Lock()
	window := a.window
	a.window = nil
	gen := a.cfg.generation
	a.mu.Unlock()
	if len(window) == 0 {
		return
	}

	r := report.From(window, report.Options{Cmdlines: a.cmdlines})
	r.Generation = gen
	r.Version = a.version

	a.mu.Lock()
	a.ring.push(r)
	a.mu.Unlock()

	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// flushLoop drains the ring whenever there is something in it, in bounded
// batches, oldest first. Failures back off with full jitter; a 429 waits
// exactly as long as the server asked.
func (a *agent) flushLoop(ctx context.Context) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		}
		for {
			batch := a.take(postBatch)
			if len(batch) == 0 {
				attempt = 0
				break
			}
			d, err := a.client.post(ctx, batch)
			if err == nil {
				a.drop(len(batch))
				a.apply(d)
				attempt = 0
				continue
			}

			var wait time.Duration
			if ra, ok := errors.AsType[retryAfterError](err); ok {
				wait = ra.delay
				log.Printf("post: %v", err)
			} else {
				attempt++
				wait = fullJitter(attempt)
				log.Printf("post failed (attempt %d, retrying in %s): %v", attempt, wait.Round(100*time.Millisecond), err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// apply acts on the server's directive. Remote configuration is refused
// unless the agent was started with -allow-remote-config; the refusal is
// logged here and, from stage 8, echoed back so the server can tell "will
// not" from "has not yet". A remote update needs -allow-remote-update,
// which does not exist until stage 8, so it is always refused.
func (a *agent) apply(d *report.Directive) {
	if d == nil {
		return
	}
	if d.Update {
		log.Printf("server asked for a self-update; refused, remote update is not supported yet")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if d.Generation == a.cfg.generation {
		return
	}
	if !a.allowRemoteConfig {
		if d.Generation != a.lastRefused {
			a.lastRefused = d.Generation
			log.Printf("server sent configuration generation %d; refused, started without -allow-remote-config", d.Generation)
		}
		return
	}

	if d.SampleSeconds > 0 {
		a.cfg.sample = max(time.Duration(d.SampleSeconds)*time.Second, time.Second)
	}
	if d.ReportSeconds > 0 {
		a.cfg.report = time.Duration(d.ReportSeconds) * time.Second
	}
	a.cfg.report = max(a.cfg.report, a.cfg.sample)
	if d.Containers != nil {
		a.cfg.containersOff = !*d.Containers
	}
	a.cfg.generation = d.Generation
	log.Printf("applied configuration generation %d: sample %s, report %s, containers %v",
		d.Generation, a.cfg.sample, a.cfg.report, !a.cfg.containersOff)
}

// take returns up to n queued reports without removing them; drop removes
// them once the server has them, so a failed post loses nothing.
func (a *agent) take(n int) []report.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ring.peek(n)
}

func (a *agent) drop(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring.drop(n)
}

func (a *agent) pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ring.len()
}

// offset is this host's stable slot within the report interval, derived
// from the host ID rather than drawn at random so it survives restarts:
// re-rolling on every start would let a rebooted fleet re-clump.
func (a *agent) offset(interval time.Duration) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(a.hostID))
	return time.Duration(h.Sum32()) % interval
}

// nextTick is the first wall-clock instant after now that lands on the
// interval grid plus offset.
func nextTick(now time.Time, interval, offset time.Duration) time.Time {
	t := now.Truncate(interval).Add(offset)
	for !t.After(now) {
		t = t.Add(interval)
	}
	return t
}
