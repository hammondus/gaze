# Roadmap

The stages gaze is being built in, in order. Tick items as they land.

For why any of this is shaped the way it is, see
[DESIGN-DECISIONS.md](DESIGN-DECISIONS.md). This file records *what* and *when*;
that one records *why*. Do not argue a decision here.

Each stage is meant to stand on its own: the tree builds, the tests pass, and
nothing half-finished is left behind a flag. Stages 1 to 3 give you something
useful on your own hosts before any of the hard parts.

**Status:** stages 1 to 8 done, except that stage 5's `hammondus/mfa`
dependency still resolves through a local `go.work`: publish the module,
pin it in go.mod, and delete go.work before the server image can build.
Stage 9 is not scheduled.

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

- [x] `cmd/gaze-server`, SQLite through `modernc.org/sqlite`, WAL, one writer.
- [x] Schema and forward-only migrations: hosts, admin accounts (see stage 5),
      per-host bearer tokens stored as unsalted SHA-256 hashes, looked up by
      index, with `last_used_at`. See "Each host's token is its own" in
      DESIGN-DECISIONS for why unsalted is correct here.
- [x] `gaze-server enroll <hostname>`: generate a token, print it once, store
      only its hash. The stage-5 web flow wraps this path; it also serves
      headless setups on its own.
- [x] Ingest endpoint: token hash lookup, a size cap enforced on the
      decompressed body, schema tolerance in both directions (see "The wire
      format is not the snapshot"), `429` with `Retry-After` under load.
- [x] Store the agent's sample time and the server's receive time per report.
      Charts and roll-ups read sample time; staleness reads receive time.
- [x] Retention and roll-up: raw 60s for 7 days, 5-minute for 90 days, hourly
      for 2 years, keeping minimum and maximum beside the mean. Roll up only
      windows older than two hours, so a backlog flushed after an outage is
      never rolled up short.
- [x] A `query` package that reconstructs a per-host view from stored
      reports. The web front end (stage 5) and the SSH TUI (stage 6) both
      call it; neither writes SQL of its own.
- [x] Dockerfile and compose file; restore the `docker-build`, `deploy`, and
      `logs` Makefile targets, acting on the server alone.
- [x] `release` builds `gaze` and `gaze-agent` only, asset names unchanged.

**Done when** a week of synthetic data for ten hosts rolls up correctly and the
database size matches the estimate in DESIGN-DECISIONS to within a factor of two.

## Stage 5 — Server: web front end

- [x] Admin login: password (Argon2id) plus mandatory TOTP through
      `github.com/hammondus/mfa`, following `mfademo`'s session and lockout
      pattern. `mfa` did not resolve, so it rides a git-ignored `go.work`
      until it is published — see the note in go.mod. Schema supports
      several admin accounts; the setup flow only provisions one for now.
      Recovery is `gaze-server admin reset`, not recovery codes — see
      DESIGN-DECISIONS.
- [x] Session cookies `Secure` and `HttpOnly`; no page renders host data to a
      request without a valid session.
- [x] Host enrolment (authenticated): a page over the stage-4 `enroll` path —
      generate a per-host token, display it once, store only its hash.
- [x] Host list with current state and last-seen time.
- [x] Host detail: the same measurements the TUI shows, over time.
- [x] Graphs, vanilla front end, no framework — server-rendered SVG, no
      JavaScript at all.
- [x] Handlers call the stage-4 `query` package; no SQL in the HTTP layer.
- [x] Every host-reported string (container name, process command, interface
      name, mount label) rendered through `html/template`, never concatenated
      into HTML. `TestStoredXSS` posts a scripted container name and asserts
      it renders inert.
- [x] Caching per the house policy: `no-cache` on HTML, hashed asset URLs
      `immutable`, `private` on anything per-session, `no-store` on the two
      pages that show a secret.
- [x] Absent fields drawn as gaps, never as zero.

**Done when** a host that has never reported, one reporting now, and one that
stopped an hour ago are each unmistakable at a glance, and no page is
reachable without signing in.

## Stage 6 — Presentation: SSH TUI

A second, optional way to look at the fleet, alongside the browser: `ssh` to
the collector and land in the same Bubble Tea interface `gaze` already
renders locally, sourced from stored reports instead of `/proc`.

- [x] Split the rendering half of `internal/ui` from the local-collection
      wiring, staying under `internal/`. The split is one type: `ui.New`
      takes a `Source` function instead of a `*metrics.Collector`, so the
      local binary passes `col.Collect` and the server passes a
      query-backed reconstructor.
- [x] Reconstruct a `metrics.Snapshot`-shaped view per host through the
      stage-4 `query` package (`query.LatestSnapshot`). Fields a report
      never carried — the full process table, per-core CPU, sensors —
      render as `Absent`, not zero: this is the collector's reduced view,
      not a remote `gaze`.
- [x] SSH server hand-rolled against `golang.org/x/crypto/ssh`, not an app
      framework: one session type, pubkey auth, a rendered view. See
      DESIGN-DECISIONS for why this is not `charmbracelet/wish`.
- [x] Generate a host key on first run and persist it beside the database. A
      restart must not change the server's SSH identity;
      `TestHostKeyPersists` pins it.
- [x] Handle `pty-req` and `window-change` requests, so resize works the way
      it does in a local terminal. These are the parts `wish` would have
      hidden; the checklist owns them instead.
- [x] Auth is public-key only, checked against an `authorized_keys`-format
      allow-list beside the database (or `-ssh-authorized-keys`), re-read
      per handshake so revocation needs no restart — no password, no MFA,
      no session cookie, and `mfa` is never touched.
- [x] One TUI session per connection; `q` disconnects rather than exiting a
      process. A session lands on the fleet list; enter opens a host in
      the ordinary dashboard, and `q` steps back before it disconnects.
- [x] Its own listen address and flag (`-ssh-addr`), off by default and
      independent of the web front end — either runs without the other.

**Done when** `ssh -p <port> gaze@server` with an authorized key drops
straight into a live view of every reporting host, and an unlisted key is
refused before a shell — or a TUI — is ever reached.

## Stage 7 — Alerting

- [x] Rule model: metric, comparison, threshold, duration. The rules are
      code in `internal/alert`, not rows — see "Alerting has two evaluation
      paths" in DESIGN-DECISIONS for why no rules table exists.
- [x] Per-host state machine: OK, PENDING, FIRING, OK. Mail on transitions
      only; state persists in `alert_state`, so a restart neither re-fires
      every open alert nor forgets one.
- [x] Threshold rules evaluated on ingest, on the newest report of a batch;
      backlog older than ten minutes describes the past and is skipped.
- [x] Staleness sweep on a timer, for hosts that have stopped reporting.
      Evaluated on receive time, not sample time, so a host with a broken
      clock can still go stale. A never-reported host is setup in
      progress, not an outage, and stays silent.
- [x] Re-notify suppression, so a long outage is not a mailbox: at most
      one message per rule per host per hour, transitions included.
- [x] Wire `github.com/hammondus/mailer` — it resolves, so it is pinned in
      go.mod like any dependency. `GAZE_SMTP_*` and `GAZE_ALERT_TO`
      configure it; unset, alerts compose into the log through `NewLog`.
- [x] Tests against `MemorySender`, with no SMTP anywhere.
      `TestFlappingHourSendsOneMessage` is the done-when, verbatim.

**Done when** a rule that flaps either side of its threshold for an hour sends
one message.

## Stage 8 — Agent management

- [x] Change interval and collection settings per host from the web front end,
      refused unless the agent was started with its own remote-changes flag —
      separate from, and independent of, `-allow-remote-update`. Process
      command-line collection is never one of the settable fields; it stays a
      local, agent-only choice regardless of this flag.
- [x] Remote-triggered self-update, refused unless `-allow-remote-update`. The
      directive is a bare trigger: no version, no URL. An agent told to update
      runs the same fetch-latest-and-verify path `gaze --update` already runs
      by hand, then re-execs into the new binary. Attempts are gated to one
      an hour on the agent, because each is a GitHub download. The unit file
      documents what enabling this does to the hardening.
- [x] The server sends the update directive only when its own version matches
      the latest release, so agents never end up running a newer schema than
      the server. Checked through the same `/releases/latest` redirect the
      updater reads, lazily — only while an update stands requested — and
      cached for an hour.
- [x] Staggered rollout, so agents do not all fetch from GitHub at once: each
      host has a stable slot, derived from its id, inside a fifteen-minute
      window.
- [x] Show each agent's version and config generation, and whether it has
      caught up with what it was told — and, distinct from "not caught up
      yet", whether it explicitly declined a directive and why. The refusal
      travels as `Report.Declined` and shows on the host list itself.

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
