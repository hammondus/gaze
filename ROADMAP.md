# Roadmap

The stages gaze is being built in, in order. Tick items as they land.

For why any of this is shaped the way it is, see
[DESIGN-DECISIONS.md](DESIGN-DECISIONS.md). This file records *what* and *when*;
that one records *why*. Do not argue a decision here.

Each stage is meant to stand on its own: the tree builds, the tests pass, and
nothing half-finished is left behind a flag. Stages 1 to 3 give you something
useful on your own hosts before any of the hard parts.

**Status:** stages 1 to 3 done. Stage 4 not started.

---

## Stage 1 — Restructure

Make room for the other binaries. No behaviour change: `gaze` looks and acts
exactly as it does now, and the existing tests pass untouched. Shared packages
stay under `internal/` — see "Everything stays under internal" in
DESIGN-DECISIONS.

- [x] Move `main.go` to `cmd/gaze/`.
- [x] Split the collector into `collector_linux.go` and
      `collector_unsupported.go` under build constraints, with only Linux
      implemented.
- [x] Add `Snapshot.Absent`, empty on Linux, and render it as a dash in the TUI.
- [x] Update the README `go install` path and the `Makefile` package paths.
- [x] Confirm `make test`, `make frame`, and `make run` behave as before.

**Done when** a rendered frame is byte-identical to one from the previous
commit, and `go vet ./...` passes for `linux/arm64`.

## Stage 2 — The wire contract

The type the agent sends and the server stores. No network code yet.

- [x] `internal/report` package: `Report`, `Directive`, and a `Schema`
      constant.
- [x] `report.From([]metrics.Snapshot, Options) Report`, reducing to scalars,
      per-interface and per-device rates, mounts, process counts, containers,
      and the top few processes. It takes the sample slice rather than one
      snapshot, because aggregation needs the series — see DESIGN-DECISIONS.
- [x] Aggregation across samples: minimum, maximum, and mean per field.
- [x] `Report` carries the span of sample times it aggregates, from the
      agent's clock. See "Reports carry the agent's clock" in
      DESIGN-DECISIONS.
- [x] Process command lines excluded by default, behind an opt-in.
- [x] Round-trip tests, and a golden JSON fixture to catch accidental schema
      changes.

**Done when** a `Report` built from the demo snapshot is under 4 KB of JSON, and
the golden fixture fails on any field rename.

## Stage 3 — Agent, Linux only

- [x] `cmd/gaze-agent`: collect on a short interval, post on a long one.
- [x] `POST /api/v1/reports`, JSON, gzipped, bearer token read from a token
      file (mode 0600), never a CLI flag. The body is always a JSON array,
      so a backlog flushes in bounded batches of ten.
- [x] Refuse a plain `http://` server URL unless the host is loopback. The
      directive channel assumes TLS.
- [x] Bounded ring buffer of unsent reports, about sixty.
- [x] Full-jitter backoff, capped near fifteen minutes; honour `Retry-After`.
- [x] Stable per-host reporting offset derived from the host ID.
- [x] Apply directives from the reply; echo the config generation. Applying
      is gated behind `-allow-remote-config` from the first release — the
      stage 8 flag, shipped early, because loosening a default is safe and
      tightening one is a behaviour break.
- [x] systemd unit, and a documented unprivileged user. The documentation
      states what Docker socket access does to that word — see "Unprivileged
      stops at the Docker socket" in DESIGN-DECISIONS.
- [x] A throwaway server that logs what it receives, to prove the path:
      `cmd/gaze-devserver`, deleted when stage 4 lands.

**Done when** an agent survives the throwaway server being stopped for ten
minutes and the gap is filled on its return.

## Stage 4 — Server: ingest and storage

- [ ] `cmd/gaze-server`, SQLite through `modernc.org/sqlite`, WAL, one writer.
- [ ] Schema and forward-only migrations: hosts, admin accounts (see stage 5),
      per-host bearer tokens stored as unsalted SHA-256 hashes, looked up by
      index, with `last_used_at`. See "Each host's token is its own" in
      DESIGN-DECISIONS for why unsalted is correct here.
- [ ] `gaze-server enroll <hostname>`: generate a token, print it once, store
      only its hash. The stage-5 web flow wraps this path; it also serves
      headless setups on its own.
- [ ] Ingest endpoint: token hash lookup, a size cap enforced on the
      decompressed body, schema tolerance in both directions (see "The wire
      format is not the snapshot"), `429` with `Retry-After` under load.
- [ ] Store the agent's sample time and the server's receive time per report.
      Charts and roll-ups read sample time; staleness reads receive time.
- [ ] Retention and roll-up: raw 60s for 7 days, 5-minute for 90 days, hourly
      for 2 years, keeping minimum and maximum beside the mean. Roll up only
      windows older than two hours, so a backlog flushed after an outage is
      never rolled up short.
- [ ] A `query` package that reconstructs a per-host view from stored
      reports. The web front end (stage 5) and the SSH TUI (stage 6) both
      call it; neither writes SQL of its own.
- [ ] Dockerfile and compose file; restore the `docker-build`, `deploy`, and
      `logs` Makefile targets, acting on the server alone.
- [ ] `release` builds `gaze` and `gaze-agent` only, asset names unchanged.

**Done when** a week of synthetic data for ten hosts rolls up correctly and the
database size matches the estimate in DESIGN-DECISIONS to within a factor of two.

## Stage 5 — Server: web front end

- [ ] Admin login: password (Argon2id) plus mandatory TOTP through
      `github.com/hammondus/mfa`, following `mfademo`'s session and lockout
      pattern. Check whether `mfa` resolves before adding it; if it does not,
      use `go.work`, never a `replace` — the same rule stage 7 applies to
      `mailer`. Schema supports several admin accounts; the setup flow only
      provisions one for now.
- [ ] Session cookies `Secure` and `HttpOnly`; no page renders host data to a
      request without a valid session.
- [ ] Host enrolment (authenticated): a page over the stage-4 `enroll` path —
      generate a per-host token, display it once, store only its hash.
- [ ] Host list with current state and last-seen time.
- [ ] Host detail: the same measurements the TUI shows, over time.
- [ ] Graphs, vanilla front end, no framework.
- [ ] Handlers call the stage-4 `query` package; no SQL in the HTTP layer.
- [ ] Every host-reported string (container name, process command, interface
      name, mount label) rendered through `html/template`, never concatenated
      into HTML.
- [ ] Caching per the house policy: `no-cache` on HTML, hashed asset URLs
      `immutable`, `private` on anything per-session.
- [ ] Absent fields drawn as gaps, never as zero.

**Done when** a host that has never reported, one reporting now, and one that
stopped an hour ago are each unmistakable at a glance, and no page is
reachable without signing in.

## Stage 6 — Presentation: SSH TUI

A second, optional way to look at the fleet, alongside the browser: `ssh` to
the collector and land in the same Bubble Tea interface `gaze` already
renders locally, sourced from stored reports instead of `/proc`.

- [ ] Split the rendering half of `internal/ui` from the local-collection
      wiring, staying under `internal/`. `cmd/gaze-server` needs the `Model`,
      not the collection loop around it.
- [ ] Reconstruct a `metrics.Snapshot`-shaped view per host through the
      stage-4 `query` package. Fields a report never carried — the full
      process table, per-core CPU — render as `Absent`, not zero: this is the
      collector's reduced view, not a remote `gaze`.
- [ ] SSH server hand-rolled against `golang.org/x/crypto/ssh`, not an app
      framework: one session type, pubkey auth, a rendered view. See
      DESIGN-DECISIONS for why this is not `charmbracelet/wish`.
- [ ] Generate a host key on first run and persist it beside the database. A
      restart must not change the server's SSH identity.
- [ ] Handle `pty-req` and `window-change` requests, so resize works the way
      it does in a local terminal. These are the parts `wish` would have
      hidden; the checklist owns them instead.
- [ ] Auth is public-key only, checked against a configured allow-list — no
      password, no MFA, no session cookie. Reuses the trust an operator
      already has SSHing into these hosts as root, and never touches `mfa`.
- [ ] One TUI session per connection; `q` disconnects rather than exiting a
      process.
- [ ] Its own listen address and flag, off by default and independent of the
      web front end — either runs without the other.

**Done when** `ssh -p <port> gaze@server` with an authorized key drops
straight into a live view of every reporting host, and an unlisted key is
refused before a shell — or a TUI — is ever reached.

## Stage 7 — Alerting

- [ ] Rule model: metric, comparison, threshold, duration.
- [ ] Per-host state machine: OK, PENDING, FIRING, OK. Mail on transitions only.
- [ ] Threshold rules evaluated on ingest.
- [ ] Staleness sweep on a timer, for hosts that have stopped reporting.
      Evaluated on receive time, not sample time, so a host with a broken
      clock can still go stale.
- [ ] Re-notify suppression, so a long outage is not a mailbox.
- [ ] Wire `github.com/hammondus/mailer`; check whether it resolves before
      adding it, and use `go.work` rather than a `replace` if it does not.
- [ ] Tests against `MemorySender`, with no SMTP anywhere.

**Done when** a rule that flaps either side of its threshold for an hour sends
one message.

## Stage 8 — Agent management

- [ ] Change interval and collection settings per host from the web front end,
      refused unless the agent was started with its own remote-changes flag —
      separate from, and independent of, `-allow-remote-update`. Process
      command-line collection is never one of the settable fields; it stays a
      local, agent-only choice regardless of this flag.
- [ ] Remote-triggered self-update, refused unless `-allow-remote-update`. The
      directive is a bare trigger: no version, no URL. An agent told to update
      runs the same fetch-latest-and-verify path `gaze --update` already runs
      by hand.
- [ ] The server sends the update directive only when its own version matches
      the latest release, so agents never end up running a newer schema than
      the server. See "The wire format is not the snapshot".
- [ ] Staggered rollout, so agents do not all fetch from GitHub at once.
- [ ] Show each agent's version and config generation, and whether it has
      caught up with what it was told — and, distinct from "not caught up
      yet", whether it explicitly declined a directive and why.

**Done when** changing one host's interval is visible as applied, not just
sent, and a declined directive is as visible on the host list as an applied
one.

## Stage 9 — Windows collector

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

- **`gaze -remote`**, the existing binary dialing the collector's query API
  directly, rather than SSHing into it. Stage 6 covers the SSH path; this
  would be the client-side alternative for someone who wants their own
  `gaze` on their own machine rather than a shell into the collector's. Not
  scheduled — revisit if stage 6 turns out not to be enough.
- **macOS collection.** Needs Mach APIs. macOS is the development machine, not a
  monitored one.
- **Signed releases.** `--update` verifies a checksum, which catches corruption
  and not a compromised release. `minisign` is the first thing to reach for if
  that ever matters.
