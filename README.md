# gaze

A system monitor for Linux terminals, modelled on
[glances](https://github.com/nicolargo/glances). It ships as one static
executable. (AMD64 and ARM64).

It only runs on linux and it does not have anywhere near the number of features of glances. I reduced it to the features that I personally need so that it:
- installs without any dependencies
- uses less RAM (approx half of glances)
- uses less CPU (approx one third glances)


```
 gaze  aurora  Linux 6.8.0-45-generic  up 6d 4h 12m                                       14:23:07  1.0s

CPU   █████████▎────────────    42%   ▁▂▃▅▆▅▄▃▃▄▅▄▄  MEM   ███████████████───────    69%   ▄▄▅▆▇▆▅▅▅▅▆▅▆
LOAD  █████████▎────────────    42%   ▁▁▂▃▄▄▃▃▂▂▃▃▃  SWAP  █████▊────────────────    26%   ▁▁▂▂▃▃▂▂▂▂▃▃▃
8 cores · load 3.40 2.90 2.10 · 11G of 16G used · 412 tasks, 1183 threads · 1 zombie · 6.1% iowait

NETWORK                         NAME                 STATE      UPTIME  ▾CPU%     MEM COMMAND
                  rx       tx   pgdata               running      6d4h    18%    2.0G postgres -c share…
eth0          1.4M/s   340K/s   edge-proxy           running      6d3h   0.4%     48M nginx -g 'daemon …
wg0           2.0K/s   900B/s
lo               0/s      0/s       PID USER        ▾CPU%   MEM%     RSS COMMAND
                                   2841 postgres      18%    12%    2.0G postgres: writer process
DISK I/O                           1102 craig         12%   8.9%    1.5G /usr/lib/firefox/firefox --pro…
                read    write       331 root         2.2%   0.4%     68M /usr/lib/systemd/systemd-journ…
nvme0n1        12M/s   4.0M/s      7734 craig        1.4%   2.1%    350M go build ./...
sda              0/s    96K/s       883 root         0.9%   1.2%    190M /usr/bin/dockerd -H fd://
                                   1544 www-data     0.6%   0.2%     24M nginx: worker process
FILESYSTEM                          412 nobody         0%     0%      0B [defunct-thing]
                  used   free
…postgresql/data   91%    12G
/home              62%   180G
q quit  c/m/s/t/p sort:cpu  v view:split  K kthreads:off  / filter  ␣ pause  ? help
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

### Updating

```
gaze --update
```

Self-updating starts at v0.2.0. A v0.1.0 binary has no `--update` flag, so
upgrade that one with the download above, once.

It reads the latest version, downloads the build for this machine, checks it
against the published `SHA256SUMS`, and replaces itself. Replacing a running
executable is safe on Linux, so you can run this from inside a `gaze` session.

If `gaze` lives somewhere only root can write, such as `/usr/local/bin`, the
update needs `sudo`. It says so rather than failing with a permissions error.

To ask without installing anything, use `gaze --check-update`. Its exit code
distinguishes all three outcomes, so a script can tell an available update
apart from a failure to check:

| Code | Meaning |
|---|---|
| 0 | This is the published version |
| 1 | A newer release exists |
| 2 | Could not find out |

```sh
gaze --check-update
case $? in
  0) ;;
  1) gaze --update ;;
  *) echo "could not reach the release server" >&2 ;;
esac
```

The checksum confirms the download arrived intact. It is not a signature, so it
does not prove who built the release.

### From source

To build from source, you need Go 1.26 or later:

```
go install github.com/hammondus/gaze/cmd/gaze@latest
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
| `v` | Cycle the split, container, and process views |
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
| `-containers` | `true` | Collect container statistics. Set `false` to leave the runtime socket alone |
| `-version` | | Print the version and exit |

To read a captured pseudo-filesystem instead of the running kernel, point
`-procfs` and `-sysfs` at a copied directory tree.

## The agent

`gaze-agent` posts the same measurements to a central `gaze-server`. It
collects every 10 seconds and reports once a minute, sending the minimum,
maximum, and mean of each figure, so a spike between reports still arrives.
During a server outage it queues about an hour of reports and fills the gap
on reconnection.

```
gaze-agent -server https://gaze.example.net -token-file /etc/gaze/token
```

| Flag | Default | Purpose |
|---|---|---|
| `-server` | | Server base URL. `https`, or `http` on loopback only |
| `-token-file` | | Bearer token file, mode `0600`. Never a flag: `ps` shows flags to every user |
| `-sample` | `10s` | Collection interval |
| `-report` | `1m` | Reporting interval |
| `-containers` | `true` | Collect container statistics |
| `-cmdlines` | `false` | Include process command lines, which can carry secrets. A local choice; no server can switch it on |
| `-allow-remote-config` | `false` | Let the server change intervals and collection settings |
| `-allow-remote-update` | `false` | Let the server trigger a self-update to the latest release. The directive carries no version and no URL |

The two `allow` flags are separate consents, both off by default. An agent
that receives a directive its flags forbid declines it and says why on its
next report, so the server's host list shows "declined" rather than an
agent that never seems to catch up. Command-line collection is never
remotely settable: `-cmdlines` stays a local choice.

To run it as a service, start from
[contrib/gaze-agent.service](contrib/gaze-agent.service): it documents the
unprivileged user, the token file, what joining the `docker` group for
container statistics really grants, and what enabling remote update does
to the unit's hardening.

## The server

`gaze-server` ingests and stores reports in SQLite: raw for 7 days, 5-minute
roll-ups for 90 days, hourly for 2 years, each keeping minimum and maximum
beside the mean so spikes survive aggregation. It serves a web front end
over them: a host list that tells a reporting host from a stale one from
one that has never reported, and per-host graphs — server-rendered SVG, no
JavaScript — with tables for filesystems, containers, and the busiest
processes. The SSH view is still to come — see [ROADMAP.md](ROADMAP.md).

It deploys as a container behind a TLS-terminating proxy and is never a
release asset:

```
echo "GAZE_KEY=$(head -c 32 /dev/urandom | base64)" >> .env
make deploy        # on the server: git pull, then docker compose up -d --build
make logs
```

`GAZE_KEY` seals the admin's TOTP secret; keep it out of the repository and
away from the database. On first start the log prints a setup code; open
`/setup`, enter it, and create the admin account — a password and a
mandatory authenticator enrolment. If the admin is ever locked out, reset
from the shell and set up again:

```
docker compose exec gaze-server gaze-server admin reset -db /data/gaze.db
```

To enroll a host, use the **Enrol a host** page, or from the shell:

```
docker compose exec gaze-server gaze-server enroll <hostname> -db /data/gaze.db
```

Both print the host's bearer token exactly once. The database stores the
token's SHA-256, never the token. Enrolling the same host again mints a
second token, which is how rotation works.

### Managing agents

Each host's page can change its agent's intervals and container
collection, and trigger a self-update — per host, or fleet-wide from the
host list, staggered so the agents do not all fetch from GitHub at once.
Everything rides on the reply to the agent's next report; the server never
opens a connection to an agent, and the agent's own `allow` flags decide
whether it complies. The host list shows whether each change was applied,
is still travelling, or was declined — and a declined directive names the
flag to change.

Update directives are sent only while the server itself is on the latest
release, so an agent can never be pushed past its server's schema.

### Alerting

The server mails on state transitions, never on conditions: CPU, memory,
swap, or a filesystem above its threshold for fifteen minutes fires once,
and fires again only after recovery. A host that stops reporting for five
minutes is the alert that matters most, and it is swept for every minute.
At most one message per rule per host per hour, so a flapping metric or a
week-long outage is one email, not a mailbox.

Configure the mail path in the environment, beside `GAZE_KEY`:

```
GAZE_SMTP_HOST, GAZE_SMTP_PORT, GAZE_SMTP_USERNAME, GAZE_SMTP_PASSWORD,
GAZE_SMTP_FROM   # the SMTP account alerts send through
GAZE_ALERT_TO    # recipients, comma-separated
```

Until those are set, alerts compose into the server log instead of a
mailbox, so the rules can be watched misfiring before they can page
anyone.

### The SSH view

The server can also serve the gaze TUI itself over SSH, drawn from stored
reports instead of `/proc`: `ssh` in, land on the fleet list, and open any
host in the same dashboard the local binary renders. It is off by default;
start the server with `-ssh-addr :2222` to turn it on.

Authentication is public keys only, checked against an
`authorized_keys`-format file beside the database (or wherever
`-ssh-authorized-keys` points). The file is re-read on every connection,
so adding or revoking a key needs no restart. The server's host key is
generated on first run and kept beside the database, so its SSH identity
survives restarts. In the session, `enter` opens a host, `q` steps back,
and `q` on the list disconnects.

## Requirements

gaze reads `/proc` and `/sys` directly, so **it runs on Linux only**. It
compiles on macOS and Windows, and the tests run there, but the binary reports
what it cannot read and exits.

Everything works as an unprivileged user. Running as root adds nothing except
the command lines of other users' processes, which the kernel otherwise hides.

### Containers

Press `v` to cycle how much of the main column containers get:

| View | Container display | Process list |
|---|---|---|
| `split` | A table above the process list, sized to the number running | Shares the column |
| `containers` | The whole column, **including containers that are not running** | Hidden |
| `processes` | Hidden | The whole column |

The sidebar of network, disk, and filesystem panels stays put in all three.

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

### The cost of container statistics

gaze makes two requests per running container per refresh, one for statistics
and one for uptime. Measured on a host with 13 containers and 188 processes,
that is 5.2 ms of CPU per collection, roughly 0.4 ms per container, against
about 1.1 ms for everything else a collection does.

To leave the socket alone entirely, run `gaze -containers=false`. The container
views then say so rather than reporting a missing runtime.

To measure it on your own host:

```
GAZE_LIVE=1 go test ./internal/metrics -run TestCollectCost -v
```

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

For the choices behind the code, see
[DESIGN-DECISIONS.md](DESIGN-DECISIONS.md). For what is built and what is
still planned, see [ROADMAP.md](ROADMAP.md).

## Layout

| Path | Contents |
|---|---|
| `cmd/gaze` | The TUI: flags and start-up |
| `cmd/gaze-agent` | Sampling loop, ring buffer, and posting |
| `cmd/gaze-server` | Ingest, the web and SSH front ends, and the `enroll` and `admin reset` commands |
| `internal/metrics` | Collection from `/proc` and `/sys`. Standard library only. |
| `internal/report` | The wire contract between agent and server. Standard library only. |
| `internal/store` | The server's schema, migrations, and every write |
| `internal/query` | Read-only reconstruction of per-host views |
| `internal/alert` | Threshold rules, staleness, and mail on transitions |
| `internal/ui` | Bubble Tea model, panels, and formatting |
| `internal/update` | `--update` and `--check-update` |

The shared type is `metrics.Snapshot`: the collector decides what the
numbers are, the display decides what they look like, and `report` reduces
a run of snapshots to what goes over the wire.

## Licence

MIT. See [LICENSE](LICENSE).
