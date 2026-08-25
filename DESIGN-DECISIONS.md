# Design decisions

This file records the choices that were not obvious, and why the alternative
lost. For what the program does, see the README. For what is built and what is
still to come, see [ROADMAP.md](ROADMAP.md).

Sections describing the agent and the server are decisions, not descriptions:
most of that code is not written yet. The roadmap says which stage each belongs
to.

## Scope

gaze started as a Linux terminal system monitor, modelled on
[glances](https://github.com/nicolargo/glances) but not a port of it, and that
is still what `gaze` the binary is: one static executable, no runtime
configuration.

It has since grown a second job. `gaze-agent` sends the same measurements to
`gaze-server`, which stores them for several hosts and draws them in a browser.
The motivation is the same as the original one. Zabbix does this and more, and
the collecting and displaying halves were too heavy to put on a small VM.

What is still out of scope: metric exporters, a plugin system, remote command
execution, and agent-to-agent anything. What was out of scope and no longer is:
a web interface and a client/server mode. See "One repository, six
components".

## One repository, six components

Everything lives in one module, `github.com/hammondus/gaze`, and builds into
three binaries.

| Component | What it is | Depends on | External dependencies |
|---|---|---|---|
| `internal/metrics` | Collection. The platform boundary lives here. | nothing | none |
| `internal/report` | The wire types, and the reduction from a snapshot. | `metrics` | none |
| `internal/ui` | The Bubble Tea model and panels — a view of a `metrics.Snapshot`, not a copy of one. | `metrics` | Bubble Tea, Lip Gloss |
| `cmd/gaze` | The TUI, over a local terminal. | `ui` | (via `ui`) |
| `cmd/gaze-agent` | Collects and posts to a server. | `metrics`, `report`, `update` | none |
| `cmd/gaze-server` | Ingests, stores, alerts, and presents — over HTTP and, optionally, SSH. | `report`, `ui` | SQLite, mailer, mfa, `golang.org/x/crypto` (SSH) |

The prose in this file names the components by their short names; the paths
are the ones in the table.

Two things about that table matter more than the layout.

**`report` is a component, not a file in one of the others.** It is the only
thing two components share and neither owns. Put it inside the agent and the
server imports the agent; put it inside the server and the agent imports the
server. Both are wrong, and the cost of finding out is paid later, when
changing a field means editing across a boundary that should not exist.

**The arrows point one way.** `report` depends on `metrics` and never the
reverse, so the collector never learns that a server exists. That is what keeps
`metrics` importable by anything, and what would let the collector be extracted
to its own module later without touching it.

The alternative was two repositories, splitting at the deployment artifact: the
TUI and the agent are static binaries dropped on a monitored host, and the
server is a service with a database behind nginx. That split is real and it is
the one to make if another developer ever appears. With one developer it buys a
cross-module version dance in exchange for tidiness, and loses the property
that a change to `report` and its two consumers is one commit that either
compiles or does not.

The worry that one repository makes the agent heavy is unfounded. Go links per
binary, so SQLite in `go.mod` puts nothing in `gaze-agent`. What it does cost is
the sentence about the dependency footprint, which now has to be stated per
binary. See "Bubble Tea earns its dependencies, and they are most of them".

**`ui` exists so `cmd/gaze-server` can present a terminal view without
duplicating one.** The server's SSH front end needs the same rendering the
local TUI uses (see "The collector is one process; presentation is swappable,
not separate"), so stage 6 splits the rendering half of `ui` from the
local-collection wiring around it. A package that takes a value and draws it
has no reason to know who is asking, whether that is a local terminal or an
SSH session into the collector.

## Everything stays under internal

All shared packages live under `internal/`, not at the module root. The three
binaries are in the one module, so `internal/` restricts none of them, and
nothing outside this repository imports gaze. A package at the root is a
promise to strangers: once it is importable, someone can import it, and
`Snapshot` — which "The wire format is not the snapshot" wants free to churn —
becomes visible breakage in someone else's build.

The asymmetry decides it. If an external consumer ever appears, promoting a
package out of `internal/` is a rename inside one repository with one
developer. Retracting a public package is not. Nothing about the extraction
scenario in "One repository, six components" needs the packages public today;
it only needs the arrows to keep pointing one way, and they do that from
inside `internal/` too.

## Metrics come from /proc and /sys, not from a library

The collector in `internal/metrics` reads the kernel's pseudo-filesystems
directly and imports nothing outside the standard library. The obvious
alternative, `github.com/shirou/gopsutil`, is a competent port of Python's
`psutil` and would have given macOS and Windows support at no effort.

It lost on two counts. First, the deploy target is Linux, so the portability
buys nothing that matters here. Second, `gopsutil` brings about seven
transitive modules and reaches into Mach APIs on darwin, which is a lot of code
to audit for what amounts to reading a dozen text files. The Linux collector is
under 900 lines and every parser fits on a screen.

The cost is real and worth stating: **the program only runs on Linux**, and the
development machine is a Mac. The next decision is what pays for that.

`gopsutil` was reconsidered when Windows came up as a possibility, and loses
again for a new reason. It would supply Windows and macOS at once, but the
`Snapshot` this project renders and stores is not the shape `gopsutil` returns,
so it would be a translation layer over a dependency tree rather than a
replacement for the collector. The Linux half already exists and is tested. See
"The platform boundary exists before the second platform does".

## Parsers take a filesystem, not a path

Every parser takes an `io.Reader` or an `fs.FS`. None of them opens a file by
absolute path, and none carries a build constraint. Only `statfs_linux.go`,
which wraps one syscall, is Linux-only, and `statfs_other.go` stubs it out.

Three things follow:

- `go test ./...` runs on macOS against the fixture tree in
  `internal/metrics/testdata`, so the edit-test loop never leaves the dev
  machine. Only `make run` needs a Linux kernel.
- `metrics.NewWithSource` accepts any `fs.FS`, so `--procfs` and `--sysfs` can
  point at a captured `/proc` directory. To debug a reading you cannot
  reproduce live, copy the tree off the machine and replay it.
- The delta arithmetic, which is where the bugs live, is tested against two
  generations of counters in `TestCollectDeltas` without a kernel involved.

`TestLiveFrame` covers what fixtures cannot. It collects from the running
kernel and renders a frame, and it is opt-in:

```
GAZE_LIVE=1 go test ./internal/ui -run TestLiveFrame -v
```

Three defects got through the fixture suite and were caught by running against
a real kernel:

- A container host lists two dozen unused `loop` and `nbd` devices, which
  buried the real disks.
- Docker bind-mounts `/etc/resolv.conf` as a file, and `statfs` reports the
  filesystem underneath it, so the same disk appeared twice.
- The start-up frame divided by a near-zero interval. See "The first frame
  reports no rates".

Fixture data would not have shown any of them.

## The snapshot carries rates, not counters

`metrics.Snapshot` is a plain value holding finished numbers. CPU percentages,
byte rates, and IOPS are computed inside the collector against the previous
reading. The display layer holds no counter history and does no arithmetic.

Almost everything on screen is a rate, so the alternative — hand the display
raw counters and let it diff — would put the same delta logic behind every
panel and make each one responsible for detecting counter resets. Instead
`Collector` owns one previous sample of everything, and `rate` handles a
counter that moves backwards in one place.

The display layer does keep its own history, in `internal/ui/sparkline.go`.
That is not a contradiction: how much of the past to draw is a display
decision, and the ring buffer holds rendered percentages, not kernel counters.

## USER_HZ is never needed for a percentage

The CPU times in `/proc/stat` and `/proc/<pid>/stat` are in units of `USER_HZ`,
which the portable interface exposes only through `sysconf(_SC_CLK_TCK)` and so
needs cgo. Reading it would mean either linking libc, which would end the
single-static-binary property, or taking a dependency for one integer.

Neither is necessary. Every percentage this program shows is one tick count
divided by another, and the unit cancels:

```
process CPU% = process ticks in the interval
               ────────────────────────────  × core count × 100
               machine ticks in the interval
```

The constant `userHZ = 100` in `internal/metrics/process.go` is used only for
the absolute `TIME+` column, where being wrong on some exotic port would misread
a cumulative figure and nothing else. It has been 100 on every Linux port since
2.6.

## Used memory is total minus available

`Memory.Used` is `MemTotal - MemAvailable`, not `MemTotal - MemFree`. The kernel
gives back page cache and reclaimable slab on demand, so free memory alone reads
as alarmingly low on a healthy machine. `MemAvailable` is the kernel's own
estimate of what a new allocation could get. On kernels before 3.14, which do
not publish it, the collector falls back to the approximation from the patch
that introduced it.

Filesystem percentages use `used / (used + available)`, which is what `df`
reports. The blocks reserved for root are neither used nor available; counting
them would under-report a filesystem that is full for everyone but root.

## Which devices and filesystems appear

Three filters, all in `internal/metrics`:

- **Partitions.** `/proc/diskstats` lists partitions beside their parent
  device. Counting both reports the same I/O twice, so only devices present in
  `/sys/block` survive.
- **Unused block devices.** A device whose counters have never moved has never
  been used. That removes the fixed set of idle `loop` and `nbd` devices a host
  carries, which otherwise fill the panel.
- **Pseudo-filesystems.** `/proc/filesystems` marks virtual filesystems
  `nodev`, so the kernel decides which types are real rather than a hard-coded
  list that goes stale. `tmpfs`, `zfs`, `overlay`, and the network filesystems
  are marked `nodev` but hold real data, so `keepPseudoFS` keeps them. Mounts
  under `/proc`, `/sys`, `/dev`, `/run`, `/snap`, and the container storage
  directories are hidden, and a device mounted at several paths appears once.

Change any of these by editing the lists at the top of `mounts.go`. There is no
configuration file, and adding one would be the wrong trade for a program whose
whole point is that it is a single file with no state.

## Docker over the socket, not the SDK

`internal/metrics/docker.go` talks to the daemon with `net/http` over a custom
Unix dialer, in about 200 lines. The official SDK brings a large dependency
tree for what is two GET requests against a documented JSON API.

Stats are fetched with `one-shot=true` and the rates are derived across
successive polls. Without `one-shot`, the daemon blocks for a second per
container while it collects its own pair of samples, which would make the
refresh interval a lie on a host running twenty containers. Deriving rates from
polls is also consistent with how every other metric here is produced.

The API version is pinned to `v1.41`, served by every daemon since 20.10, so a
host upgrade cannot change the response shape underneath you.

Podman needs no separate client. It serves the same endpoints on its own socket
through its Docker compatibility layer, so only the path and the name shown on
screen differ. **This is not verified against a running Podman daemon** —
Podman is not installed on the development machine. What is tested is the
discovery that would find it: the candidate order, the runtime name, and the
refusal to accept a stale regular file at a socket path. If Podman turns out to
need more than a different socket, that is where the change goes.

A `tcp://` value in `DOCKER_HOST` is ignored rather than half-honoured. This
program reads the machine it runs on; a remote daemon would report containers
whose CPU and memory bear no relation to the gauges above them.

### Container polling is over half the collection cost

Measured with `TestCollectCost` on a host with 13 containers and 188
processes: 9.5 ms of CPU per collection with container polling, 4.3 ms without.
So the two requests per container cost 5.2 ms, or 0.4 ms each.

Since kernel threads stopped being read in full, the rest of a collection costs
about 1.1 ms, so container polling is now the larger share by some way on any
host running more than a handful.

Wall-clock time for the same collections was 45 ms against 4 ms. That 41 ms
gap is almost entirely time blocked on the daemon socket, which costs latency
and nearly no CPU. Timing a collection with a stopwatch therefore reports the
socket round-trip and calls it work; `TestCollectCost` reports both numbers
side by side so the two cannot be confused. They answer different questions:
CPU is what shows up when comparing against another monitor, and wall time
matters only if it approaches the refresh interval.

`-containers=false` skips the whole thing. No socket is probed and no request
is made. That exists for the measurement above, for hosts running enough
containers that the cost matters, and because not everyone wants a monitor
talking to their Docker socket.

A container view with collection switched off says so, rather than reporting
that no runtime was found. Those are different facts and one must not be
reported as the other.

### Uptime costs a second request, and that is the right call

`Uptime` needs `State.StartedAt`, which only the inspect endpoint carries.
`Created` is in the list response already, but it records when the container was
made, which for a long-lived container is a different and much older moment.

Measured against a live daemon with nine containers, inspect costs 3.9 ms
against 4.5 ms for the stats call already being made beside it. So it is
fetched every poll rather than cached. A cache keyed by container ID would go
stale the moment a container restarted — the ID does not change — and report an
uptime that never corrected itself. Paying 4 ms to avoid a wrong number that
never heals is not a trade worth making.

Both requests run in the same bounded pool, so the concurrency limit counts
containers being polled rather than requests in flight. They run in sequence
within each container's goroutine, which doubles the latency depth: issuing
them concurrently would roughly halve the 45 ms wall time measured above. That
is worth doing if the wall time ever approaches the refresh interval, and it
would not change the CPU cost.

### Block I/O folds case, and ignores the totals

`blkio_stats.io_service_bytes_recursive` reports a value per device per
operation. The operation name is capitalised under cgroup v1 and lower case
under v2, so the comparison folds case. Only `read` and `write` are summed:
cgroup v1 also emits `sync`, `async`, and `total` rows, and adding `total`
would double-count everything. Some storage drivers report nothing and leave
the array null, which reads as zero.

### Stopped containers are fetched always and shown once

The list is fetched with `all=1` on every poll, so a stopped container is
always available. Stats and inspect are only requested for running ones, so the
cost of the extra rows is a slightly larger JSON body and nothing else.

Only the dedicated container view shows them. In the other two views the
container list shares the screen with everything else, and a row of dashes for
something that exited last week earns none of that space.

Whatever the sort column, a container that is not running sorts last. Its rates
are all zero, so it would otherwise scatter through the list and push the
running ones off the screen.

## Bubble Tea earns its dependencies, and they are most of them

Bubble Tea and Lip Gloss bring about twenty modules between them.

State the footprint per binary, not per repository. `gaze-agent` has no
external dependencies at all. `gaze-server` carries SQLite, the mailer, `mfa`,
and — once the SSH TUI in stage 6 lands — Bubble Tea and Lip Gloss too, through
`ui`. `gaze` carries only Bubble Tea and Lip Gloss, same as before `ui` was
split out; it just imports them one package away rather than directly.
`make release` is where the footprint is worth checking, not `go.sum`.

The alternative is an alt-screen, raw mode, and ANSI by hand, in under a
hundred lines. What Bubble Tea supplies for the extra weight is key decoding,
resize handling, mouse input, and a render loop that does not tear. For a
program whose whole job is a redrawing full-screen dashboard, that is the right
trade. Lip Gloss does nearly all the visual work and is separable from Bubble
Tea if that judgement changes.

Both are pinned to their v1 lines, for now. v2 is stable — GA since February
2026, on active patch releases since — and the API is a real improvement: a
declarative view instead of scattered program options, a saner key and mouse
model. Nothing gaze does needs what it adds beyond that, though: no
hyperlinks, no gradient borders, no keyboard-enhancement protocol. The
migration is not a rename. `View()`'s return type changes, the key-message
types and constants change, and the whole `AdaptiveColor` background-detection
story is replaced by a message round-trip (`tea.RequestBackgroundColor` /
`tea.BackgroundColorMsg`) that has the same first-frame timing sensitivity
already documented in "The first frame reports no rates". It touches
`main.go`, most of `internal/ui`, and every test that constructs a
synthetic key message. Move to v2 once stage 1 lands, as its own commit — not
bundled with a restructure whose whole point is a byte-identical frame.

## Collections are chained, not scheduled

The next collection is scheduled only once the previous snapshot arrives, in
`Model.Update`. A fixed timer would be the obvious approach and is wrong twice
over: `Collector` is not safe for concurrent use, and a machine too loaded to
keep up would queue work it never finishes, which is exactly when you are
looking at the screen. Chaining means a slow machine refreshes less often and
the numbers stay honest.

Each collection runs under a three-second timeout. Reading `/proc` cannot hang;
a wedged Docker socket can.

The interval is clamped to between 0.5 and 10 seconds. Below half a second the
cost of collecting starts to appear in the readings, so the program would be
measuring itself.

## The first frame reports no rates

`New` primes the counters and `Init` collects straight away, so the screen
fills without waiting out an interval. That leaves the first snapshot with an
interval of about a millisecond, and every rate here is a difference divided by
elapsed time. Over a millisecond one jiffy of CPU time reads as several hundred
percent.

Below `minRateInterval`, which is 100 milliseconds, the collector reports the
absolute values it read — memory, load, filesystems, the process table — and
leaves every derived figure at zero. It still stores the sample, so the next
collection diffs against it and reports real figures.

This was found by running the binary under a pseudo-terminal, not by the tests:
the start-up frame showed 50 percent CPU and a process at 200 percent, and
every frame after it was correct. `TestFirstCollectionHasNoRates` covers it
now.

## The frame is two columns, not four stacked bands

Below the gauges the screen divides into a sidebar of network, disk, and
filesystem panels, about a quarter of the width, and a main column holding the
container and process tables.

The two halves are there because the things in them want opposite shapes. An
interface, a block device, and a mount are each a short name and two figures, so
those panels are narrow and want height. A process row is a command line, so its
table is the reverse. Stacking them, which is what this did before, forces one
budget on both: the panels were rationed against the process list, and every row
given to a filesystem was a row taken from a process. Side by side they compete
for width, which they have, instead of for height, which they do not.

Two things follow from the split:

- **The sidebar takes the column's whole height**, so a tall terminal shows nine
  mounts rather than nine lines of nothing. Rows are handed out by what each
  panel has to show, not evenly: the usual machine has two interfaces and a
  fistful of filesystems, and an even split would pad the first while cutting
  the last in half. `share` in `model.go` does that, and the rows one panel
  cannot use go round again.
- **The process table is narrower**, so it drops a column or two sooner than it
  used to. That is the price, and it is paid in the columns worth least: the
  table already drops from the least useful upwards, and `COMMAND` still flexes
  into whatever is left.

Under about 90 columns there is not enough width for both. `sidebarWidth`
returns zero, and the frame falls back to the older stacked layout, where the
panels flow across the full width above the tables. A 30-column sidebar on an
80-column terminal would leave the process table 48 columns, which is not enough
for a command line, so on a narrow screen stacking is the better answer rather
than a fallback to apologise for.

The sensor panel is dropped when the sidebar is too short to give all four
panels two rows each. Three lists you can read beat four that are mostly title.

## Three container views on one key

Containers get three amounts of the main column, cycled with `v`: a table above
the process list, the whole column, or none of it.

They are one key rather than three because they are points on a single axis —
how much of the screen containers deserve — and a cycle makes that legible in
the footer, which always names the current view. The sidebar does not change
between them: it holds nothing to do with containers, so there is nothing for
the cycle to move.

The split view is the default, and its table is sized to the number of
containers running, so a host with none looks exactly like the process view
without being told to. The container view is the only one that shows containers
which are not running, and the only one with a cursor: the split view's table is
a readout, not a list you move through.

Containers used to have a fourth home, a panel in the band beside network and
disk. The sidebar that replaced the band is too narrow to say anything useful
about a container — a name and two figures, with no room for state or uptime —
and the split view's table says all of it. The panel is gone rather than
squeezed.

## One table implementation, two tables

`internal/ui/table.go` holds the column layout: which columns fit a given
width, which to drop first, how to mark the sort column, and how to keep a
cursor on screen. Both the process table and the container table are values of
`table[T]`.

The alternative was a second copy of the drop-columns loop. That loop is the
only thing standing between a narrow terminal and a wrapped line, and two
copies of it would drift. The columns themselves stay declarative: a label, a
width, the terminal width below which the column goes, and a function from the
row to a string.

## The frame is measured before it is drawn

`Model.layout` sizes every region before anything renders. A full-screen
program that emits one line too many scrolls its own header off the top on
every refresh, and one column too many wraps a line and slides the layout down
the screen. Both failures are invisible until they are not.

Three mechanisms enforce this:

- Both columns are built to the same height budget, and neither is allowed to
  exceed it. Within the main column the process table keeps at least five rows,
  and the container table above it is dropped before that minimum is touched.
- Process table columns carry a `minWidth` and are dropped from the least
  useful upwards until the command column has room, so a narrow terminal
  degrades rather than wraps.
- `View` clips its own output to the terminal height as a last resort. On a
  screen too short to hold the gauges and the footer together there is no body
  left to give up, and the arithmetic above has nothing left to trade.
- `TestViewFitsTerminal` and `TestViewFitsHeight` render every variant at seven
  terminal sizes and fail on a single overrun.
  `TestSidebarSitsBesideTheTables` covers the one failure those two miss: a
  frame that has quietly gone back to stacking still fits the terminal.

Anything already carrying colour goes through `clipWidth`, which measures
display width. The plain helpers in `format.go` count runes, and a rune count
of a styled string counts the escape sequences too.

## Sort ties break on a second column

On an idle machine every process reads zero percent. Falling straight back to
PID fills the screen with low-numbered kernel threads and buries everything you
started, so sorting by CPU breaks ties on memory, and sorting by memory breaks
ties on CPU. PID is the last resort and is always reached, so the order never
shuffles between refreshes.

## Kernel threads are identified by the kernel's own flag

A kernel thread runs entirely inside the kernel and has no user-space address
space. It holds no memory, can never be swapped, and has an empty
`/proc/<pid>/cmdline`, which is why the table shows its name in brackets — the
convention `ps` and `top` use for a name taken from the `comm` field rather
than from a real command line. On a typical host these outnumber everything you
started; on the container used for testing, 146 processes out of 163.

`K` hides them. Lower-case `k` moves the cursor, so the binding takes the
shifted key, which is also what htop uses.

Detection reads `PF_KTHREAD` from the flags field of `/proc/<pid>/stat`, which
the parser already reads. The two common alternatives are guesses that break:

- **An empty command line** also describes a zombie, whose address space the
  kernel has already torn down. Bracketed and hidden must not be the same set,
  and `TestHideKernelThreads` pins that down.
- **A parent of PID 2** misses a kernel thread whose parent has exited, and
  needs a special case for PID 2 itself.

Kernel threads are hidden by default, because on a typical host they are most
of the process table and none of the work. The context line always reports how
many are hidden, so nothing disappears without the screen saying so.

## Kernel threads cost one file read, not three

gaze reads three files per process: `stat`, `status`, and `cmdline`. For a
kernel thread the last two hold nothing. It has no user-space address space, so
no command line, no `VmSwap` line, and no user context other than root. The
kernel thread flag is in `stat`, which is read first, so the other two can be
skipped outright.

That is the largest single saving in a collection, because kernel threads are
most of the process table. Measured back to back in one run on a host with 163
processes, 148 of them kernel threads:

```
read every process fully  |  3.19 ms
skip kernel thread extras |  1.10 ms
```

A 65% cut, and nothing on screen changes: the owner is still resolved through
`userName(0)` rather than hard-coded, the name still comes from `stat`, and a
kernel thread still renders bracketed with zero swap.

`TestKernelThreadsSkipPointlessReads` asserts on the reads rather than the
values, using a filesystem wrapper that counts every `Open`. Asserting on
values would pass just as well if the files were read and thrown away, which
is the thing being fixed.

## Swap is read from status, which pays for itself

`Process.Swap` comes from `VmSwap` in `/proc/<pid>/status`, the only
per-process swap figure the kernel publishes. A process is not swapped out as a
whole; some fraction of its anonymous memory is on disk, and this is that
figure.

Reading a second file for every process on every refresh looked expensive
enough to make lazy, so it was measured first: 0.78 ms for 159 processes,
against 0.67 ms for the `stat` reads already happening. At a one second refresh
that is not worth a flag.

It also pays for itself. `status` carries the `Uid` line, so the owning user now
comes from a file already being read, and the separate `stat` call on
`/proc/<pid>` that used to supply it is gone — along with the two
platform-specific files that wrapped it.

A kernel thread has no address space and so no `VmSwap` line, and a kernel
built without `CONFIG_SWAP` omits it everywhere. Both read as zero, which is
the truth in both cases.

The `SWAP` column is coloured against the process rather than against the
machine: any swap at all is worth seeing, and zero is not. A threshold on a
percentage would colour every row identically on a machine with no swap, and
every row red on one that is thrashing.

## Errors do not clear the screen

A collector that fails records the error in `Snapshot.Errs` and leaves its
field zero. The footer shows the count and the help overlay lists them. One
unreadable file must not cost you the rest of the display, which is what you
opened the program for.

## Makefile

`build` compiles for the dev machine and confirms it builds; it does not
produce something that runs there. `run` cross-compiles the real artifact and
runs it under Docker against a Linux kernel. `release` builds `linux/arm64` and
`linux/amd64` only, because there is nothing to ship for macOS or Windows.

`docker-build`, `deploy`, and `logs` were absent while there was no service to
deploy. `gaze-server` is that service, so the three targets exist and act on it
alone. `release` keeps building the two static binaries; the server ships as a
container image and is never a release asset.

`CGO_ENABLED=0` throughout. With cgo on, `os/user` resolves names through the
host's NSS and the binary stops being portable across distributions, which
defeats the point of shipping one file.

## Distribution is a release and a self-update, not an apt repository

An apt repository was measured before it was ruled out. The mechanics are
small: a `.deb` is about 20 lines of shell and comes out at 2.0 MB with no
declared dependencies, and a signed repository is another 25 lines of
`apt-ftparchive` and `gpg --clearsign` over six static files that GitHub Pages
would host for nothing.

The setup is not the problem. The ongoing cost is:

- **The signing key becomes a permanent liability.** When it expires, every
  user's `apt update` fails with a signature error until they install a new key
  by hand. Lose it and you cannot publish; leak it and someone else can publish
  as you.
- **Every release must regenerate and re-sign the metadata, and must never
  half-fail.** A `Release` file that disagrees with its `Packages` index breaks
  `apt update` for everyone until it is fixed.
- **It serves Debian and Ubuntu only.** Alpine, RHEL, Arch, and anything in a
  container get nothing.

And it would buy almost nothing. Docker needs an apt repository because Docker
has real dependencies, systemd units, configuration in `/etc`, and several
interlocking packages. gaze is one static file in `/usr/bin` with no
dependencies, no configuration, and no service. Strip out dependency
resolution, maintainer scripts, and configuration handling, and all that is
left is `apt upgrade` noticing a new version.

`gaze --update` provides that for about 250 lines, no infrastructure, no key to
protect, and it works on every distribution rather than two.

## The updater uses no API

`internal/update` makes plain web requests. The version comes from the redirect
that `/releases/latest` issues to `/releases/tag/<version>`, read from the
`Location` header without following it. The binary and `SHA256SUMS` come from
`/releases/latest/download/<name>`.

GitHub documents a limit of 60 unauthenticated REST API calls per hour per IP
address. Reading the redirect avoids the API altogether, so that limit never
applies. The `User-Agent` names the program, its version, and the project URL,
so anyone looking at their logs can tell what is calling and where to complain.

There is deliberately **no version check at start-up**. A monitor should not
make a network request to draw its first frame, and a check on every launch
would be both slow and rude. Checking is something you ask for.

### Release asset names are now a contract

Every installed gaze asks for `gaze-linux-<arch>` and `SHA256SUMS` under
`/releases/latest/download/`, and every installed agent asks for
`gaze-agent-linux-<arch>` beside them. Those names are a compatibility surface
with every copy already on someone's machine.

Renaming an asset, or dropping `SHA256SUMS`, breaks `--update` for everyone
already installed, and their only route back is a manual download. Add new
assets alongside the existing names rather than renaming them.

The wire format is now a second such surface, and a stricter one, because an
agent that fails to parse a reply keeps running. See "The wire format is not
the snapshot".

### The replacement is a create-and-rename

The download is written to a temporary file **in the target's own directory**,
not in `/tmp`, because a rename only works within one filesystem and `/tmp` is
frequently a separate one. The mode is set to 0755 before the rename, since
`os.CreateTemp` produces 0600 and an installed binary at 0600 is not runnable.

Order matters, and it is: check the directory is writable, fetch the expected
checksum, download while hashing, compare, and only then rename. A download
that fails verification is removed and never reaches the target path;
`TestApplyRejectsABadChecksum` asserts exactly that.

`os.Rename` is atomic within a filesystem, and Linux keeps a running program's
inode alive after its path is replaced, so updating from inside a running gaze
session is safe.

The checksum and the binary come from the same server, so it proves the
download arrived intact and nothing more. It is not a signature and does not
establish who built the release. Adding `minisign` would fix that for far less
work than an apt repository, and is the first thing to do if that ever matters.

## The wire format is not the snapshot

`report.Report` is a separate type from `metrics.Snapshot`, and
`report.From` reduces one to the other. Sending a `Snapshot` would have been
less code and it is the wrong answer twice over.

**Size.** A `Snapshot` is shaped for a display redrawing once a second: the
whole process table, per-core CPU, cumulative counters beside the rates derived
from them. On a host with 400 processes that is on the order of 150 KB of JSON.
At one report a minute it comes to several gigabytes per host per year, which is
the resource problem that made Zabbix unusable here, rebuilt from scratch. A
report carrying scalars, per-interface and per-device rates, mounts, process
*counts*, and the few busiest processes is a couple of KB, and a few hundred
bytes gzipped.

**Rate of change.** `Snapshot` is internal to this project and should stay free
to churn as the display grows. A wire format is a contract with agents already
installed and with rows already in a database. Those two things want opposite
freedoms, so they are two types.

The reduction is one-way and lossy on purpose. Recording only the top few
processes rather than all of them is the single biggest sizing decision in the
system: a row per process per interval is what makes monitoring databases
enormous, and it is almost never what you go back and read.

Four decisions from building it, recorded here rather than rediscovered:

- **`From` takes the sample slice, not one snapshot.** The signature is
  `From([]metrics.Snapshot, Options) Report`. The alternative was reducing
  each snapshot and merging finished reports, and it loses twice: minimum,
  maximum, and mean need the series, and the busiest processes can only be
  ranked fairly by matching PIDs across samples. Holding a report interval's
  snapshots in memory costs the agent about a megabyte. A name missing from
  one sample counts as zero for it, which is what it contributed then; a PID
  recycled within the one-minute span folds two processes into one row, which
  is bounded enough not to be worth carrying kernel start times to prevent.
- **Fast movers aggregate; slow movers carry their last observation.** CPU,
  load, memory and swap use, and the per-interface, per-device, and
  per-container rates are `Stat{Min, Max, Mean}`. Mount capacity and the
  process counts are the latest reading: over a minute an envelope on those
  is three copies of nearly the same number, and the interesting extremes
  show up in load and CPU anyway.
- **Containers are in the report.** The original reduction list left them
  out, but stage 5 renders container names on the host pages, so they have to
  arrive somehow. Name, image, state, aggregated CPU, and memory; the
  runtime name and the disabled flag travel alongside, because "switched
  off", "no runtime", and "none running" must stay three different facts on
  the server too.
- **Wire figures round to two decimals.** A mean that is a full float64
  doubles the bytes of every figure it carries, and nothing downstream needs
  micro-percent.

The demo report — six samples of a host with three interfaces, two disks,
five mounts, seven containers, and a full process table — is 3.4 KB of JSON
and about 1.5 KB gzipped, against the 4 KB budget in ROADMAP stage 2.

`Report` carries a `Schema` integer. A server must accept an older schema than
it knows, because an agent updates when it is told to, not when the server does.

The reverse also happens, because the update directive fetches the latest
release rather than a named version: a server that is itself a release behind
would push its agents past itself. Two rules cover it. On a newer schema than
it knows, the server stores the fields it does know and records the mismatch,
so a fresher agent degrades to a partial report rather than looking like a
dead host — silently rejecting it would be indistinguishable from an outage on
the host list. And the server sends the update directive only while its own
version matches the latest release, checked through the same
`/releases/latest` redirect the updater already reads, so it never creates the
situation deliberately.

## Sample often, report rarely

The agent collects on a short interval and posts on a long one, sending the
minimum, maximum, and mean of each field over the samples in between. The
default is a 10-second sample and a 60-second report.

Collecting and posting at the same 60 seconds would be simpler and would throw
away exactly what a monitor is for. Every rate here is a difference over the
interval, so a 60-second collection yields a 60-second average, and a CPU spike
lasting 20 seconds does not appear at all. Sampling six times as often costs six
times a collection, which is about 1.1 ms of CPU each, and nothing in bandwidth.

The same reasoning applies to the roll-up tiers in the database, and for the same
reason those keep minimum and maximum beside the mean. See "SQLite, and what six
months costs".

## Reports carry the agent's clock, and the server keeps its own

A `Report` states the span of sample times it aggregates, read from the
agent's clock. That is what makes the ring buffer worth having: a backlog
flushed after an outage lands at the time it describes, not the time it
arrived. Stamping reports on arrival would file an hour of buffered history
into one minute and leave the outage looking like a flat line followed by a
spike.

The server stores its own receive time beside the sample time. The two
disagree exactly when something interesting happened — an outage, a backlog,
or a broken clock — so neither can substitute for the other:

- **Charts and roll-ups read sample time.** They describe the host.
- **Staleness reads receive time.** It exists to catch a host that has gone
  silent, and a host whose clock is wrong must not be exempt from it.

Two consequences for ingest and roll-up:

- **A sample time ahead of the server's clock is clamped to receive time.**
  A future-dated row sits in a roll-up window that has not happened yet and
  corrupts it silently. Clamping keeps the report and loses only the claim
  the agent's clock was not entitled to make. Sample times in the past are
  accepted as far back as the raw retention window, which covers the agent's
  one-hour buffer many times over.
- **The roll-up only processes windows older than two hours** — twice the
  buffer horizon — so a backlog cannot arrive after its window was already
  rolled up short. The raw tier is kept seven days, so the delay costs
  nothing.

## Absent is not zero

`Snapshot.Absent` names the fields the running platform cannot supply. A
consumer renders those as a dash, and the server stores NULL.

Linux supplies everything today, so `Absent` is empty and the field looks like
dead weight. It is there because the moment a second platform exists it stops
being: Windows publishes no per-process swap figure and no vendor-independent
sensor API, and without a third state both arrive as `0`.

In the TUI a wrong zero is cosmetic. In a database it is a lie with a timestamp
on it, and after six months there is no way to separate "this host had no swap
in use" from "nobody ever asked". The distinction cannot be recovered later, so
it has to exist before the first row is written.

This is the same distinction the container code already makes between
`ContainersDisabled` and an empty `ContainerRuntime`, one level up. Switched
off, not found, and not supported are three facts, and reporting any one of them
as another sends you looking for a problem you do not have.

## The platform boundary exists before the second platform does

`metrics` is split into `collector_linux.go` and `collector_unsupported.go`
under build constraints, with `Snapshot` as the platform-neutral contract
between them. Only Linux is implemented. There is no Windows support and none is
scheduled.

The boundary is thinner than those file names suggest, and deliberately so:
it is `NewSource`, the one constructor that knows how to observe the running
machine. The `Collector` machinery cannot carry a build constraint, because
the fixture tests drive it on macOS through `NewWithSource` — constraining it
would end the macOS edit-test loop that the next section exists to protect.
On a platform with no implementation, `NewSource` returns a source whose
every read fails with an error naming the platform, so the failure lands in
`Snapshot.Errs` as one clear sentence rather than as a missing `/proc`.

The boundary is drawn anyway because the cost is asymmetric. Drawn now, while
one platform fills it, it is a file split and a build tag. Drawn later, after an
agent and a server depend on the package, it is a refactor of everything that
imports it, plus the `Absent` problem above with rows already in the database.

The parsers keep the shape described in "Parsers take a filesystem, not a path".
They stay Linux-only in their assumptions and free of build constraints in their
signatures, so the fixture tests keep running on macOS.

What a Windows collector would actually take, recorded so the estimate is not
made again from scratch: there is no `/proc`, so none of the parsing is reusable
and the work is Win32 through `golang.org/x/sys/windows` —
`NtQuerySystemInformation` for processes and per-core CPU, `GlobalMemoryStatusEx`
for memory, `GetIfTable2` for interfaces, `GetLogicalDriveStrings` and
`GetDiskFreeSpaceEx` for filesystems, `DeviceIoControl` for disk I/O. Roughly 600
to 1000 lines, with no fixture replay and no macOS edit-test loop. Avoid WMI: a
query can take hundreds of milliseconds, which defeats the point. Sensors and
per-process command lines would be `Absent`, the latter because reading another
process's PEB needs elevation.

macOS collection is not planned at all. It needs Mach APIs, and macOS is the
development machine rather than a monitored one.

## The agent is told what to do in the reply

The server never opens a connection to an agent. It answers the agent's POST
with a directive: a reporting interval, what to collect, and optionally an
instruction to update.

That keeps the whole system at plain request and response. A WebSocket or a
long poll would let the server push, at the cost of a connection to nurse
through restarts and proxies, and there is nothing to push that cannot wait
until the next report. It also means an agent behind NAT needs no inbound path,
which is most of them.

Because every directive rides on the reply, the transport is the trust
boundary: the agent refuses a plain `http://` server URL unless the host is
loopback. Everything below about what a spoofed server can and cannot do
assumes TLS is doing its job.

**The directive carries a generation number and the agent echoes it back.** The
server learns whether a change actually took effect, which is the difference
between configuration and hope when an agent has been offline for an hour or
someone has rolled a binary back.

**A remote update is refused unless the agent was started with
`-allow-remote-update`, and the default is off.** Be precise about what the
directive can and cannot do: it is a bare trigger, carrying no version and no
URL, so an agent told to update runs exactly the code path `gaze --update`
already runs by hand — resolve GitHub's `/releases/latest` redirect, fetch,
verify the checksum, replace. A compromised or spoofed server can make an
opted-in agent update sooner than it otherwise would; it cannot hand the agent
an arbitrary binary or pin it to an old, vulnerable one, because nothing in the
directive names a target. That would need compromising the GitHub release
itself, an identical risk for a manual update today. This has to stay true as
the directive grows: no future field may let it name a version or a source. A
"pin this host to version X" feature, if it is ever wanted, needs its own
design rather than a value slotted into this one. Requiring a flag makes
enabling remote update a deliberate act, recorded in the unit file. Rollouts
are staggered for the separate reason that ten agents updating at once all
fetch from GitHub at once. The server also withholds the directive entirely
while it is itself behind the latest release, so it never pushes its agents
past its own schema — see "The wire format is not the snapshot".

**Remote configuration changes are a second, independent flag, off by default
the same way.** An agent that accepts remote interval and collection-target
changes has not thereby accepted remote updates, and the reverse. Keeping them
separate means an operator comfortable with one is not forced into the other
to get it.

**Process command-line collection is never one of the fields either flag can
touch.** It stays local, set only by whoever configures the agent on that
host — see "The wire format is not the snapshot" for why it is opt-in at all.
Command lines can carry secrets typed on them, and the consent to send those
is not the same consent as letting a server nudge a sampling interval.

**A declined directive is echoed back too, with why.** An agent that refuses a
remote update or a remote configuration change because its start flags do not
allow it reports that refusal on its next POST. Without it, the server cannot
tell an agent that will never comply from one that is simply offline — both
would show as "has not caught up" on the host list, and only one of them needs
a person to walk over and change a flag.

## Each host's token is its own

Every agent authenticates with its own bearer token, generated when its host
is enrolled — through `gaze-server enroll <hostname>` at the command line
from stage 4, or the authenticated web page that wraps the same path from
stage 5 — rather than one shared secret handed to every agent. The CLI form
exists first because ingest needs testable tokens a stage before the web
front end exists, and it stays because a headless setup should not need a
browser. A compromised host then costs that host's
token, not the whole fleet's — the agent process is unprivileged and reads
only metrics, so the worst a stolen token buys an attacker is the ability to
post fabricated reports as that one host, not read or affect any other.

The server stores a SHA-256 hash of the token, looked up by index on ingest,
never the token itself. A token is 256 bits of randomness, generated once and
shown once; a timing side-channel on the hash lookup gives an attacker
nothing to walk toward the way it would against a short, human-chosen secret,
so no comparison beyond an indexed hash lookup is needed. This is the same
shape as `mfa`'s recovery codes: a deterministic hash and a lookup, not a
stored secret to protect.

The hash is deliberately unsalted, and that is a decision rather than an
omission. A salt makes a guessable secret expensive to attack in bulk, which
256 random bits are not, and a salted hash cannot be found by index — every
lookup would become a scan that hashes the presented token against each row's
salt in turn. Unsalted-and-indexed is the standard shape for high-entropy
API tokens.

The agent reads its token from a file, `-token-file`, mode 0600, owned by the
unprivileged service user — never a CLI flag, which `ps` would show to every
local user on the host.

Revoking a token is a row delete, not a rotation of anything shared. The
server also keeps `last_used_at` per token, which costs nothing to store and
gives the host list in stage 5 "this host has not reported in a week" for
free.

## Unprivileged stops at the Docker socket

Reading `/proc` and `/sys` needs no privilege, so the agent's service user
can be a plain account with no shell. Container stats break that story. They
need the runtime socket, which on most hosts means membership of the
`docker` group — and the docker group is root in practice, because anyone in
it can start a container with the host filesystem mounted inside.

So with container collection on, "unprivileged" describes the account, not
the blast radius: a compromised agent in the docker group owns the host. The
systemd unit documentation states this plainly instead of implying the word
covers it. The configurations where the word is honest are `-containers=false`,
which never touches a socket, and rootless Podman, whose socket grants only
that user's containers. The earlier claim in "Each host's token is its own" —
that a stolen token buys only fabricated reports — is about the token and
remains true; this is about the agent process itself.

## Signing in reuses mfa, not a new auth stack

Every page on `gaze-server` sits behind a session that needs a password and a
TOTP code, through `github.com/hammondus/mfa` in the shape `mfademo` already
demonstrates: Argon2id for the password, `mfa.VerifyTOTP` for the code, a
session cookie marked `Secure` and `HttpOnly`, and consecutive-failure
lockout on the account rather than the IP. Writing a second, weaker auth
stack for this project would be strictly worse than depending on one that
already has the replay window, the lockout, and the recovery-code path worked
out and tested.

The threat this defends against is stated plainly: an administrator who
already has full access to every monitored machine is not who this is for.
It is for keeping that access from becoming anyone else's — a browser tab
showing every host's containers, mounts, and running processes is worth more
to an attacker than most of the individual machines behind it.

TOTP is mandatory, not optional. A password-only login on the one page that
aggregates every host would undercut the reason a second factor exists
anywhere else in the fleet.

**The schema supports several admin accounts; the setup flow provisions
exactly one.** A second administrator is not implausible, and retrofitting a
users table after rows already reference a single hard-coded admin is the
expensive direction. Building the table now and shipping a one-account setup
page costs nothing extra and avoids that migration later. Whether or how to
add a second admin — an invite flow, an admin-adds-admin page — is undecided
and does not need deciding until it is needed.

The AES key that seals TOTP secrets (`mfa.Seal`/`mfa.Open`) comes from an
environment variable set at deploy time, never a file written beside the
database. `mfademo`'s auto-generated `key.dev` is a convenience for trying the
demo, explicitly not a pattern to copy — a key stored next to what it
encrypts protects nothing.

## Host-reported strings are untrusted input

A container name, a process command line, an interface name, and a mount
label all originate on the machine being monitored, not on `gaze-server`, and
every one of them is free-form text chosen by whatever created that container
or process. `report.From` does not sanitise them, and it should not: they are
data, and what they mean is exactly what the operator asked to see.

They stop being safe the moment stage 5 renders them into an HTML page.
Anyone who can name a Docker container on a monitored host —
`<img src=x onerror=...>` is a legal container name — can put a script into
whatever browser next opens that host's page. That page is signed-in
territory: a second-factor-protected session with a view over every host in
the fleet is a worse thing to hand an attacker via stored XSS than most of
the hosts individually.

Every template that renders a host-reported field goes through
`html/template`, never string concatenation or `text/template`, and no
handler writes response bytes directly for anything that includes reported
data. This is a rule to check on every new page stage 5 adds, not a control
applied once at the start.

## Two herds, and they need different fixes

**On recovery**, backoff is jittered across the full range:
`sleep = rand(0, min(cap, base * 2^attempt))`, capped at about fifteen minutes.
Plain exponential backoff keeps agents that failed together retrying together,
because they are all computing the same delay from the same outage. The
randomness is the part that decorrelates them, so it is not a refinement of the
backoff, it is the point of it. Backlogs go up in bounded batches, and the
server slows an agent with `429` and `Retry-After` rather than by falling over.

**In steady state**, a fixed 60-second interval puts every agent on the minute
boundary, because they were installed by the same script. Each agent adds an
offset derived from a hash of its host ID. Derived rather than random, so it is
stable across restarts and the load stays spread instead of re-clumping.

The buffer that survives an outage is a bounded ring, about sixty reports, an
hour at the default interval. Bounded is the important word: an unbounded buffer
converts a long server outage into an out-of-memory kill on the host you were
trying to watch, which is a monitoring system causing the incident.

## SQLite, and what six months costs

Storage is SQLite through `modernc.org/sqlite`, in WAL mode, with a single
writer goroutine. That driver is pure Go, so `CGO_ENABLED=0` survives and the
server stays one static binary in its container.

The alternative was fixed-width append-only files, one per host per day, which
is about 92 KB per host per day and has no dependency at all. It loses on
queries: retention, roll-ups, and alert evaluation are all things SQL does in a
line and hand-rolled storage does in a page. `modernc.org/sqlite` is a large
dependency, and it is on the server, which is the component that can afford one.

Concurrency needs no thought at this scale. Ten hosts at one report a minute is
one write every six seconds.

Sizing, from row arithmetic rather than measurement. Ten hosts, 60-second
reports, 182 days is 2.6 million host-level rows, and at roughly 150 bytes a row
with indexes that is about 400 MB. The per-interface, per-device, and per-mount
series are the bulk: a host with three interfaces, two disks, and four mounts
adds nine rows an interval, or about another 1.4 GB. Call it 2 to 3 GB for six
months at full resolution.

That would fit, and the roll-up exists anyway:

| Tier | Resolution | Retention |
|---|---|---|
| raw | 60s | 7 days |
| five-minute | 5m | 90 days |
| hourly | 1h | 2 years |

It costs little to write now and cannot be backfilled later, and it turns six
months into two years for under 500 MB. Rolled-up rows keep the minimum and
maximum beside the mean, or every spike disappears the moment the raw tier
expires, which is exactly when you would go looking for one.

## The collector is one process; presentation is swappable, not separate

`cmd/gaze-server` ingests, stores, alerts, and — through `ui` — presents, in
one binary with two optional listeners: HTTP for the web front end, and SSH
for a TUI. Neither is a separate deployable artifact, and getting a swappable
front end does not require one to be.

The precedent is already in this document. `report` is its own package
specifically so that neither the agent nor the server has to import the
other's internals; the same argument applies one level in. A `query` package
that reconstructs a per-host view from stored reports is the seam both
front ends call. That seam is what makes them swappable — a Go interface, not
a process boundary or a second wire format.

**If the two ever need to be separate processes, SQLite pays for the split
before it happens.** WAL mode already allows one writer and many readers on
the same file. Two binaries on one host, one writing and one only reading,
share the database directly — no query API, no new auth boundary, no new
wire format to version. That is the escape hatch, kept open by not writing
anything today that would foreclose it, rather than built in advance for a
need that has not shown up. Splitting onto *different* hosts is a different
question and does need a network API; nothing here is designed to make that
hard later, but nothing is being built for it now either.

**Presentation is a reduced view, not a second `gaze`.** Both the web pages
and the SSH TUI are built from `report.Report` rows, and `report` is
deliberately lossy — see "The wire format is not the snapshot". The full
process table and per-core CPU never reach the collector, so they render as
`Absent` in both front ends. Neither is a substitute for `ssh` to the host
itself and running `gaze` there; both are a fleet-level view, which is what
they are for.

## The SSH TUI is hand-rolled against x/crypto/ssh, not an app framework

`charmbracelet/wish` is the obvious library for "serve a Bubble Tea program
over SSH" — it is from the same vendor as Bubble Tea and Lip Gloss, and it
does exactly this. Measured directly (`go list -m all` against a scratch
module), it pulls in 66 transitive modules, `golang.org/x/crypto` among them
doing the actual protocol work underneath. That is not a decision the size of
picking a rendering library; authenticating and terminating SSH sessions is
close in kind to picking a TLS stack, and it gets the same scrutiny "Docker
over the socket, not the SDK" already applies to a Docker client: read the
documented protocol library directly rather than take on an application
framework built over it.

The server here needs one thing: accept a connection, check the client's
public key against a configured allow-list, and hand the session to the same
`ui.Model` a local terminal would get. That is a few hundred lines against
`golang.org/x/crypto/ssh` — the library `wish` itself depends on for the
protocol — not the multiplexed-session, multi-app framework `wish` is built
to support. `x/crypto` is a `golang.org/x` package: not the standard library,
but maintained by the Go team under the same review standard, and already the
way Go programs implement SSH without cgo.

Hand-rolling means owning the parts `wish` would have hidden, and the stage 6
checklist names them: a host key generated on first run and persisted beside
the database, because a server that changes identity on restart trains every
operator to accept host-key warnings; and `pty-req` and `window-change`
handling, so resize behaves the way a local terminal does.

**Authentication is public-key only, against an allow-list, and never touches
`mfa`.** An SSH session authenticated by a private key is a different and, for
this operator, stronger model than a browser session: it is the same key
already trusted for root access to every monitored host, not a second
credential to phish or leak. The web front end needs a password and TOTP
because a browser session is bearer-token-shaped and reachable by anyone who
finds the URL; an SSH session is neither. Requiring MFA here as well would add
a second factor to a login that is already stronger than the thing MFA
usually protects.

## Alerting has two evaluation paths, and one of them is a timer

Threshold rules are evaluated when a report is stored, not on a schedule. The
work is already in memory, and the alert fires on the report that crossed the
line rather than up to a minute later.

Staleness cannot work that way. "No report from this host in three intervals"
is a statement about a report that did not arrive, so nothing triggers its
evaluation and it needs a periodic sweep. It is also the most valuable rule in
the system, because it is the one that fires when a machine dies, so the sweep
is not an afterthought to the event-driven path.

Every rule holds a state machine per host: OK, PENDING, FIRING, back to OK, with
a duration before PENDING becomes FIRING and mail only on a transition.
Evaluating a bare threshold every minute and mailing on every true result is how
alerting becomes a folder you stop reading. "Above 90 percent for fifteen
minutes" is a different and far more useful statement than "above 90 percent".

Mail goes through `github.com/hammondus/mailer`. `MemorySender` lets the whole
alerting path be tested with no SMTP anywhere, and `NewLog` composes and logs in
development, which is what you want the first fifty times a rule misfires.

## Known gaps

- **No process actions.** glances can send a signal to a process. Ending a
  process is destructive and a monitor is the wrong place to do it by accident,
  so the cursor selects but does nothing. Adding it needs a confirmation step,
  not just a key binding.
- **No GPU, IRQ, or per-process I/O panels.** All three are available under
  `/proc` and `/sys` and none is written.
- **The TUI has no alert history.** glances keeps a log of threshold
  crossings. `gaze` keeps 60 samples per gauge for the sparklines and nothing
  else; history and alerting belong to the server.
- **No Windows collection**, and no macOS collection. The boundary is drawn and
  nothing is behind it. See "The platform boundary exists before the second
  platform does".
- **`gaze`, run locally, cannot read from a server.** Stage 6 renders a
  reconstructed `Snapshot` from the collector, but only inside an SSH session
  into it. `gaze -remote` — the local binary dialing a query API directly, so
  someone can point their own `gaze` at a collector without SSHing in — is
  still unscheduled. See "The collector is one process; presentation is
  swappable, not separate" and the "Not scheduled" entry in ROADMAP.md.
- **Releases are not signed.** `gaze --update` verifies a checksum, which
  catches corruption but not a compromised release, and a remote-triggered
  update carries the identical exposure since it reuses the same fetch path —
  see "The updater uses no API" and "The agent is told what to do in the
  reply". `minisign` is the first thing to reach for if that risk ever
  outweighs the cost of a signing key.
- **cmdline is read for every process on every refresh.** That is one extra
  file read per process, a few milliseconds for a few hundred processes. If it
  ever shows up in a profile, sort first and read the command line only for the
  rows on screen.
