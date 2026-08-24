# Roadmap

The stages gaze is being built in, in order. Tick items as they land.

For why any of this is shaped the way it is, see
[DESIGN-DECISIONS.md](DESIGN-DECISIONS.md). This file records *what* and *when*;
that one records *why*. Do not argue a decision here.

Each stage is meant to stand on its own: the tree builds, the tests pass, and
nothing half-finished is left behind a flag. Stages 1 to 3 give you something
useful on your own hosts before any of the hard parts.

**Status:** stage 1 not started. Everything before it is the existing TUI.

---

## Stage 1 — Restructure

Make room for the other binaries. No behaviour change: `gaze` looks and acts
exactly as it does now, and the existing tests pass untouched.

- [ ] Promote `internal/metrics` to `metrics`, a public package.
- [ ] Move `main.go` to `cmd/gaze/`.
- [ ] Split the collector into `collector_linux.go` and
      `collector_unsupported.go` under build constraints, with only Linux
      implemented.
- [ ] Add `Snapshot.Absent`, empty on Linux, and render it as a dash in the TUI.
- [ ] Update the README `go install` path and the `Makefile` package paths.
- [ ] Confirm `make test`, `make frame`, and `make run` behave as before.

**Done when** a rendered frame is byte-identical to one from the previous
commit, and `go vet ./...` passes for `linux/arm64`.

## Stage 2 — The wire contract

The type the agent sends and the server stores. No network code yet.

- [ ] `report` package: `Report`, `Directive`, and a `Schema` constant.
- [ ] `report.From(metrics.Snapshot, Options) Report`, reducing to scalars,
      per-interface and per-device rates, mounts, process counts, and the top
      few processes.
- [ ] Aggregation across samples: minimum, maximum, and mean per field.
- [ ] Process command lines excluded by default, behind an opt-in.
- [ ] Round-trip tests, and a golden JSON fixture to catch accidental schema
      changes.

**Done when** a `Report` built from the demo snapshot is under 4 KB of JSON, and
the golden fixture fails on any field rename.

## Stage 3 — Agent, Linux only

- [ ] `cmd/gaze-agent`: collect on a short interval, post on a long one.
- [ ] `POST /api/v1/reports`, JSON, gzipped, bearer token.
- [ ] Bounded ring buffer of unsent reports, about sixty.
- [ ] Full-jitter backoff, capped near fifteen minutes; honour `Retry-After`.
- [ ] Stable per-host reporting offset derived from the host ID.
- [ ] Apply directives from the reply; echo the config generation.
- [ ] systemd unit, and a documented unprivileged user.
- [ ] A throwaway server that logs what it receives, to prove the path.

**Done when** an agent survives the throwaway server being stopped for ten
minutes and the gap is filled on its return.

## Stage 4 — Server: ingest and storage

- [ ] `cmd/gaze-server`, SQLite through `modernc.org/sqlite`, WAL, one writer.
- [ ] Schema and forward-only migrations.
- [ ] Ingest endpoint: auth, body size cap, schema-version tolerance,
      `429` with `Retry-After` under load.
- [ ] Retention and roll-up: raw 60s for 7 days, 5-minute for 90 days, hourly
      for 2 years, keeping minimum and maximum beside the mean.
- [ ] Dockerfile and compose file; restore the `docker-build`, `deploy`, and
      `logs` Makefile targets, acting on the server alone.
- [ ] `release` builds `gaze` and `gaze-agent` only, asset names unchanged.

**Done when** a week of synthetic data for ten hosts rolls up correctly and the
database size matches the estimate in DESIGN-DECISIONS to within a factor of two.

## Stage 5 — Server: web front end

- [ ] Host list with current state and last-seen time.
- [ ] Host detail: the same measurements the TUI shows, over time.
- [ ] Graphs, vanilla front end, no framework.
- [ ] Caching per the house policy: `no-cache` on HTML, hashed asset URLs
      `immutable`, `private` on anything per-session.
- [ ] Absent fields drawn as gaps, never as zero.

**Done when** a host that has never reported, one reporting now, and one that
stopped an hour ago are each unmistakable at a glance.

## Stage 6 — Alerting

- [ ] Rule model: metric, comparison, threshold, duration.
- [ ] Per-host state machine: OK, PENDING, FIRING, OK. Mail on transitions only.
- [ ] Threshold rules evaluated on ingest.
- [ ] Staleness sweep on a timer, for hosts that have stopped reporting.
- [ ] Re-notify suppression, so a long outage is not a mailbox.
- [ ] Wire `github.com/hammondus/mailer`; check whether it resolves before
      adding it, and use `go.work` rather than a `replace` if it does not.
- [ ] Tests against `MemorySender`, with no SMTP anywhere.

**Done when** a rule that flaps either side of its threshold for an hour sends
one message.

## Stage 7 — Agent management

- [ ] Change interval and collection settings per host from the web front end.
- [ ] Remote-triggered self-update, refused unless `-allow-remote-update`.
- [ ] Staggered rollout, so agents do not all fetch from GitHub at once.
- [ ] Show each agent's version and config generation, and whether it has caught
      up with what it was told.

**Done when** changing one host's interval is visible as applied, not just sent.

## Stage 8 — Windows collector

Not scheduled. Build it if people ask; there is no local need for it.

- [ ] `collector_windows.go` through `golang.org/x/sys/windows`.
- [ ] Sensors, per-process swap, and command lines reported as `Absent`.
- [ ] Windows service wrapper for the agent.
- [ ] Windows build of the TUI, which should be close to free by then.
- [ ] Add `gaze-agent-windows-amd64` alongside the existing release assets.

**Done when** a Windows host appears in the front end with dashes, not zeroes,
where the platform cannot answer.

---

## Not scheduled

- **`gaze -remote`**, the TUI drawing a host from the server rather than local
  `/proc`. Cheap once `report` exists, and worth not foreclosing.
- **macOS collection.** Needs Mach APIs. macOS is the development machine, not a
  monitored one.
- **Signed releases.** `--update` verifies a checksum, which catches corruption
  and not a compromised release. `minisign` is the first thing to reach for if
  that ever matters.
