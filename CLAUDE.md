# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

gaze is a single-binary system monitor for Linux terminals, modelled on glances.
It reads `/proc` and `/sys` directly, ships as one static executable for
linux/arm64 and linux/amd64, and has no runtime configuration.

Before making a non-obvious choice, read `DESIGN-DECISIONS.md`. It records why
each alternative lost, and it is the file to update when you make such a choice.

`ROADMAP.md` lists the stages the project is being built in, and which are done.
gaze is growing from one TUI into three binaries — the TUI, an agent, and a
server that stores reports from several hosts. Most of that is not written yet.
Check the roadmap before assuming a package exists.

## Commands

```sh
make test      # go vet + go test, plus vet of the linux/arm64 build
make frame     # print one rendered frame from synthetic data (TestRenderDemo)
make run       # cross-compile and run the real binary under Docker, against a Linux kernel
make release   # build dist/gaze-linux-{arm64,amd64} + SHA256SUMS
make publish   # attach release artifacts to a GitHub release for the current tag
```

Single test: `go test ./internal/ui -run TestViewFitsTerminal -v`

Opt-in tests that need a real kernel or daemon (skip everywhere else):

```sh
GAZE_LIVE=1 go test ./internal/ui -run TestLiveFrame -v        # collect from the running kernel, render a frame
GAZE_LIVE=1 go test ./internal/metrics -run TestCollectCost -v  # measure collection cost against the live Docker socket
GAZE_SYNTH=1 go test ./internal/store -run TestSyntheticWeek -v  # write a synthetic week for ten hosts; the stage 4 done-when proof
GAZE_DEMO_DB=/tmp/demo.db go test ./cmd/gaze-server -run TestSeedDemo  # seed a database for walking the web pages by hand
```

The full test suite runs on macOS: collection is tested against the fixture
tree in `internal/metrics/testdata` (a copied `/proc` and `/sys`), and
rendering against a synthetic snapshot. Only `make run` and the `GAZE_LIVE`
tests need Linux.

## Architecture

`metrics.Snapshot` is the shared type: the collector decides what the numbers
are, the display decides what they look like, and `report` reduces a run of
snapshots to what goes over the wire.

- `cmd/gaze/main.go` — flags, start-up checks, `--update`/`--check-update`
  handling.
- `cmd/gaze-agent` — samples on a short interval, posts reductions on a long
  one. The loop, ring buffer, and HTTP client live in `package main`; nothing
  else consumes them. Test with `go test ./cmd/gaze-agent`.
- `cmd/gaze-server` — ingest endpoint, roll-up sweep, the web front end
  (login, setup, host pages, server-rendered SVG graphs — no JavaScript),
  and the `enroll` and `admin reset` subcommands. Needs `GAZE_KEY` in the
  environment. Ships as a container image (`Dockerfile`, `compose.yml`),
  never a release asset. Until `hammondus/mfa` is published, building needs
  the git-ignored `go.work` (see the note in go.mod); the Docker build does
  not work yet.
- `internal/store` — the server's schema, forward-only migrations, and every
  write: enrolment, ingest, roll-up, retention. SQLite through
  `modernc.org/sqlite`, the server's one storage dependency.
- `internal/query` — read-only reconstruction of per-host views for
  presentation. The stage-5 web front end and stage-6 SSH TUI call it;
  neither writes SQL.
- `internal/metrics` — collection from `/proc`, `/sys`, and the container
  runtime socket. **Standard library only; do not add a dependency here.**
- `internal/report` — the wire contract between the agent and the server:
  `Report`, `Directive`, and the reduction from snapshots. **Standard library
  only.** Shipped fields are never renamed; the golden fixture in
  `internal/report/testdata` fails on any change to the JSON.
- `internal/ui` — Bubble Tea model, panels, tables, formatting. Bubble Tea and
  Lip Gloss, pinned to their v1 lines, are the only external dependencies the
  `gaze` binary carries; state the dependency footprint per binary, not per
  repository.
- `internal/update` — self-update from GitHub releases, using plain web
  requests (no GitHub API, to stay clear of its rate limit).

Constraints that shape most changes:

- **Parsers take an `io.Reader` or `fs.FS`, never an absolute path**, and carry
  no build constraints (only `statfs_linux.go`/`statfs_other.go` do). This is
  what keeps the edit-test loop on macOS and lets `-procfs`/`-sysfs` replay a
  captured tree. Keep new parsers in this shape and add fixtures under
  `internal/metrics/testdata`.
- **`Snapshot` carries finished rates, not counters.** All delta arithmetic and
  counter-reset handling lives in `Collector`, which owns the one previous
  sample. The UI's only history is the sparkline ring buffer of rendered
  percentages.
- **Collections are chained, not on a timer**: `Model.Update` schedules the
  next collection only after the previous snapshot arrives. `Collector` is not
  safe for concurrent use.
- **The frame is measured before it is drawn.** `Model.layout` sizes every
  region; columns and panels are dropped to fit rather than allowed to wrap.
  `TestViewFitsTerminal` and `TestViewFitsHeight` fail on a single overrun —
  run them after any rendering change.
- **A failing collector records into `Snapshot.Errs` and leaves its field
  zero.** Errors never clear the screen.
- **CGO is off everywhere** (`CGO_ENABLED=0` in the Makefile). Linking libc
  would end the static-binary property.
- **Release asset names are a compatibility contract.** Every installed gaze
  fetches `gaze-linux-<arch>` and `SHA256SUMS` from
  `/releases/latest/download/`. Add new assets alongside these names; never
  rename them.

Container stats (`internal/metrics/docker.go`) talk to the Docker or Podman
socket with `net/http` over a Unix dialer, API pinned to v1.41 — not the SDK.
Podman works through its Docker compatibility layer but is not verified against
a live daemon.
