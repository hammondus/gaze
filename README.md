# gaze

A system monitor for Linux terminals, modelled on
[glances](https://github.com/nicolargo/glances). It ships as one static
executable with no runtime dependencies.

```
 gaze  aurora  Linux 6.8.0-45-generic  up 6d 4h 12m                    14:23:07  1.0s

CPU   ████████████▋─────────────────    42%   ▁▂▃▅▆▅▄▃▃▄▅▄▄  MEM   ████████████████████▌────    69%
LOAD  ████████████▊─────────────────    42%   ▁▁▂▃▄▄▃▃▂▂▃▃▃  SWAP  ███████▉──────────────────    26%
8 cores · load 3.40 2.90 2.10 · 11G of 16G used · 412 tasks, 1183 threads · 1 zombie · 6.1% iowait

NETWORK                                 DISK I/O                                FILESYSTEM
                          rx       tx                           read    write                  used      free
eth0                  1.4M/s   340K/s   nvme0n1                12M/s   4.0M/s   … postgresql     91%       12G
wg0                   2.0K/s   900B/s   sda                      0/s    96K/s   /                34%      420G

    PID USER        ▾CPU%   MEM%     RSS    SWAP     TIME+  THR S  COMMAND
   2841 postgres      18%    12%    2.0G      0B   2:17.00    4 R  postgres: writer process
   1102 craig         12%   8.9%    1.5G    340M   4:34.00    5 S  /usr/lib/firefox/firefox
```

## What it shows

- CPU, load, memory, and swap gauges, each with a minute of history
- Per-core CPU gauges
- Network throughput per interface
- Disk throughput per device
- Filesystem usage
- Temperatures, fan speeds, and battery charge
- Container CPU, memory, disk I/O, network, uptime, and PID count, from Docker
  or Podman, in three views
- A sortable, filterable process table, including per-process swap

## Install

Download the binary, check it against the published checksum, and put it on
your path:

```sh
case "$(uname -m)" in
  aarch64|arm64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)"; exit 1 ;;
esac
base=https://github.com/hammondus/gaze/releases/latest/download
curl -fsSL "$base/gaze-linux-$arch" -o "gaze-linux-$arch"
curl -fsSL "$base/SHA256SUMS" -o SHA256SUMS
grep "gaze-linux-$arch" SHA256SUMS | sha256sum -c
sudo install -m755 "gaze-linux-$arch" /usr/local/bin/gaze
```

The checksum step is worth keeping. It confirms you received the bytes that
were built, and it works with both GNU coreutils and the BusyBox `sha256sum`
that Alpine ships.

The binaries are static, so they run on any Linux of that architecture,
including a `scratch` or `alpine` container. There is nothing to install
alongside them and nothing to configure.

### From source

To build from source, you need Go 1.26 or later:

```
go install github.com/hammondus/gaze@latest
```

To build the release artifacts yourself, run `make release`.

## Run

```
gaze
```

### Keys

| Key | Action |
|---|---|
| `q`, `esc` | Quit |
| `c`, `m`, `s`, `t`, `p`, `n`, `u` | Sort processes by CPU, memory, swap, time, PID, name, or user |
| `c`, `m`, `t`, `i`, `n` | Sort containers by CPU, memory, uptime, disk I/O, or name |
| `v` | Cycle the dashboard, split, and container views |
| `1` | Toggle per-core gauges |
| `K` | Show or hide kernel threads, which start hidden |
| `/` | Filter processes by name or command line |
| `space` | Pause collection |
| `+`, `-` | Halve or double the refresh interval |
| `↑` `↓`, `j` `k`, `pgup`, `pgdn` | Move through the process list |
| `?`, `h` | Help, and any collector errors |

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `-i` | `1s` | Refresh interval |
| `-procfs` | `/proc` | Path to the proc filesystem |
| `-sysfs` | `/sys` | Path to the sys filesystem |
| `-version` | | Print the version and exit |

To read a captured pseudo-filesystem instead of the running kernel, point
`-procfs` and `-sysfs` at a copied directory tree.

## Requirements

gaze reads `/proc` and `/sys` directly, so **it runs on Linux only**. It
compiles on macOS and Windows, and the tests run there, but the binary reports
what it cannot read and exits.

Everything works as an unprivileged user. Running as root adds nothing except
the command lines of other users' processes, which the kernel otherwise hides.

### Containers

Press `v` to cycle three views:

| View | Container display | Process list |
|---|---|---|
| `dashboard` | One panel in the band: name, CPU, memory | Full height |
| `split` | Full-width table above the process list | Shares the height |
| `containers` | Full-width table, **including containers that are not running** | Replaced |

For container statistics, gaze needs read access to a runtime socket. It
tries these in order, and the first that is a socket wins:

1. `DOCKER_HOST`, when it names a `unix://` path
2. `CONTAINER_HOST`, when it names a `unix://` path
3. `/var/run/docker.sock`
4. `$XDG_RUNTIME_DIR/podman/podman.sock`
5. `/run/podman/podman.sock`

Podman needs no separate support: it serves the same endpoints through its
Docker compatibility layer. To expose the rootless socket, run
`systemctl --user enable --now podman.socket`.

A remote daemon over TCP is out of scope, so a `tcp://` value is ignored.
Without a reachable runtime, the container views say so rather than showing an
empty table.

## Develop

```
make test     # vet and test, including the linux/arm64 build
make frame    # print one rendered frame from synthetic data
make run      # run the real binary against a Linux kernel, under Docker
make release  # build dist/gaze-linux-{arm64,amd64}
```

The tests run on macOS. Collection is tested against the fixture tree in
`internal/metrics/testdata`, and the display is tested against a synthetic
snapshot, so neither needs a Linux kernel. To collect from a running kernel and
render the result, run:

```
GAZE_LIVE=1 go test ./internal/ui -run TestLiveFrame -v
```

For the choices behind the code, see [DESIGN-DECISIONS.md](DESIGN-DECISIONS.md).

## Layout

| Path | Contents |
|---|---|
| `main.go` | Flags and start-up |
| `internal/metrics` | Collection from `/proc` and `/sys`. Standard library only. |
| `internal/ui` | Bubble Tea model, panels, and formatting |

The two packages share one type, `metrics.Snapshot`. The collector decides what
the numbers are, and the display decides what they look like.

## Licence

MIT. See [LICENSE](LICENSE).
