# Design decisions

This file records the choices that were not obvious, and why the alternative
lost. For what the program does, see the README.

## Scope

gaze is a Linux terminal system monitor, modelled on
[glances](https://github.com/nicolargo/glances) but not a port of it. It ships
as one static executable and has no runtime configuration, no web interface,
no client/server mode, and no metric exporters.

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
So the two requests per container cost 5.2 ms, about 54% of the total, or
0.4 ms each.

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

Bubble Tea and Lip Gloss bring about twenty modules between them. That is the
entire external dependency footprint of the project; the collector has none.

The alternative is an alt-screen, raw mode, and ANSI by hand, in under a
hundred lines. What Bubble Tea supplies for the extra weight is key decoding,
resize handling, mouse input, and a render loop that does not tear. For a
program whose whole job is a redrawing full-screen dashboard, that is the right
trade. Lip Gloss does nearly all the visual work and is separable from Bubble
Tea if that judgement changes.

Both are pinned to their v1 lines.

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

## Three container views on one key

Containers get three amounts of screen, cycled with `v`: a panel in the band,
a full-width table above the process list, and a table that replaces it.

They are one key rather than three because they are points on a single axis —
how much of the screen containers deserve — and a cycle makes that legible in
the footer, which always names the current view. Only the dashboard view puts
containers in the band; the other two drop that panel, or the same containers
would appear twice on one screen.

The container view is the only one that shows containers which are not running,
and the only one with a cursor: the split view's table is a readout, not a list
you move through.

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

- The middle band is built to a height budget. The process table keeps at least
  five rows, and the optional panels are dropped before that minimum is
  touched.
- Process table columns carry a `minWidth` and are dropped from the least
  useful upwards until the command column has room, so a narrow terminal
  degrades rather than wraps.
- `TestViewFitsTerminal` and `TestViewFitsHeight` render every variant at six
  terminal sizes and fail on a single overrun.

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

`docker-build`, `deploy`, and `logs` from the house target set are absent.
There is no service to deploy and no compose file to build, and stub targets
that print a message are worse than no targets.

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
`/releases/latest/download/`. Those three names are a compatibility surface
with every copy already on someone's machine, and they are the only one this
project has: there is no config file, no on-disk state, and no wire format.

Renaming an asset, or dropping `SHA256SUMS`, breaks `--update` for everyone
already installed, and their only route back is a manual download. Add new
assets alongside the existing names rather than renaming them.

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

## Known gaps

- **No process actions.** glances can send a signal to a process. Ending a
  process is destructive and a monitor is the wrong place to do it by accident,
  so the cursor selects but does nothing. Adding it needs a confirmation step,
  not just a key binding.
- **No GPU, IRQ, or per-process I/O panels.** All three are available under
  `/proc` and `/sys` and none is written.
- **No alert history.** glances keeps a log of threshold crossings. This keeps
  60 samples per gauge for the sparklines and nothing else.
- **Releases are not signed.** `gaze --update` verifies a checksum, which
  catches corruption but not a compromised release. See "The updater uses no
  API".
- **cmdline is read for every process on every refresh.** That is one extra
  file read per process, a few milliseconds for a few hundred processes. If it
  ever shows up in a profile, sort first and read the command line only for the
  rows on screen.
