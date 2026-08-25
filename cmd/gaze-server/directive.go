package main

import (
	"context"
	"hash/fnv"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/gaze/internal/update"
)

// staggerWindow spreads a fleet's update fetches: each host gets a stable
// slot inside it, derived from its id, so ten agents told to update do not
// all hit GitHub in the same minute. Reports arrive about once a minute,
// so a fifteen-minute window is about one fetch per slot tick.
const staggerWindow = 15 * time.Minute

// directives assembles the server's reply to a report: the desired
// configuration while the agent has not echoed it, and the update trigger
// while one is due. Everything it says rides on the reply — the server
// never opens a connection to an agent.
type directives struct {
	store *store.Store
	gate  *updateGate
}

func newDirectives(s *store.Store, version string) *directives {
	return &directives{store: s, gate: newUpdateGate(version)}
}

// For returns the directive for one host's POST, or nil when the server
// has nothing to say — which is the steady state, answered with 204.
func (d *directives) For(ctx context.Context, hostID int64) (*report.Directive, error) {
	cfg, err := d.store.HostConfig(ctx, hostID)
	if err != nil {
		return nil, err
	}

	dir := &report.Directive{}
	send := false

	// Configuration is re-sent until the agent echoes the generation —
	// or forever, for an agent whose flags refuse it; its declined text
	// is what tells the operator which of those is happening.
	if cfg.Generation > 0 && cfg.Echoed != cfg.Generation {
		dir.Generation = cfg.Generation
		dir.SampleSeconds = cfg.SampleS
		dir.ReportSeconds = cfg.ReportS
		dir.Containers = cfg.Containers
		send = true
	}

	if !cfg.UpdateAsked.IsZero() {
		latest, sendable := d.gate.check()
		switch {
		case latest != "" && cfg.AgentVersion == latest:
			// Caught up: the request is spent, however it was satisfied.
			if err := d.store.ClearUpdateRequest(ctx, hostID); err != nil {
				return nil, err
			}
		case sendable && time.Since(cfg.UpdateAsked) >= updateSlot(hostID):
			dir.Update = true
			send = true
		}
	}

	if !send {
		return nil, nil
	}
	if dir.Generation == 0 {
		// An update-only directive still carries a generation; echoing the
		// agent's own makes the configuration half a no-op.
		dir.Generation = cfg.Echoed
	}
	return dir, nil
}

// updateSlot is this host's stable position inside the stagger window.
func updateSlot(hostID int64) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(strconv.FormatInt(hostID, 10)))
	return time.Duration(h.Sum32()) % staggerWindow
}

// updateGate answers whether update directives may be sent at all: only
// while this server's own version matches the latest release, so a server
// that is itself behind never pushes its agents past its own schema — see
// "The wire format is not the snapshot" in DESIGN-DECISIONS.md.
//
// The lookup reads the same /releases/latest redirect the updater uses (no
// API, no rate limit to sit under), is checked only while an update stands
// requested, and is cached for an hour either way — a fleet reporting every
// minute must not turn into a minutely poll of GitHub.
type updateGate struct {
	version string
	lookup  func() (string, error)

	mu      sync.Mutex
	latest  string
	checked time.Time
}

func newUpdateGate(version string) *updateGate {
	u := &update.Updater{Repo: update.Repo, Version: version}
	return &updateGate{version: version, lookup: u.Latest}
}

// check returns the latest published version ("" while unknown) and
// whether updates may be sent.
func (g *updateGate) check() (latest string, sendable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.checked) >= time.Hour {
		g.checked = time.Now() // errors wait the hour out too; GitHub is not a retry loop
		v, err := g.lookup()
		if err != nil {
			log.Printf("update gate: %v", err)
		} else {
			g.latest = v
		}
	}
	// A "dev" build never equals a release tag, so a development server
	// never sends updates — which is correct: its schema is nobody's.
	return g.latest, g.latest != "" && g.latest == g.version
}
