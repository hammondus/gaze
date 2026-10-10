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

NETWORK           5 hidden (V)  NAME                 STATE      UPTIME  ▾CPU%     MEM COMMAND
                  rx       tx   pgdata               running      6d4h    18%    2.0G postgres -c share…
eth0          1.4M/s   340K/s   edge-proxy           running      6d3h   0.4%     48M nginx -g 'daemon …
wg0           2.0K/s   900B/s
                                    PID USER        ▾CPU%   MEM%     RSS COMMAND
                                   2841 postgres      18%    12%    2.0G postgres: writer process
DISK I/O          2 hidden (V)     1102 craig         12%   8.9%    1.5G /usr/lib/firefox/firefox --pro…
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
- Network throughput per interface, with the container runtime's bridges and
  veths hidden until you press `V`
- Disk throughput per device, with the loop devices hidden until you press `V`
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
| `V` | Show or hide virtual devices — loop, veth, bridges — which start hidden |
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

### Installing the agent

The agent ships as a release binary, so each host downloads it; nothing
builds on the host. To install it as a systemd service, follow these steps
on each host, including the machine that runs the server. Every step runs
as root, in one shell, because later steps use the variables earlier ones
set. Start that shell first:

```sh
sudo -i
```

1. Enroll the host on the server and copy the token it prints. See
   [enrolling a host](#enrolling-a-host). The token prints once only.

2. Download the binary and check it against the published checksum:

   ```sh
   case "$(uname -m)" in
     aarch64|arm64) arch=arm64 ;;
     x86_64|amd64)  arch=amd64 ;;
     *) echo "unsupported architecture: $(uname -m)"; exit 1 ;;
   esac
   base=https://github.com/hammondus/gaze/releases/latest/download
   curl -fsSL "$base/gaze-agent-linux-$arch" -o "gaze-agent-linux-$arch"
   curl -fsSL "$base/SHA256SUMS" -o SHA256SUMS
   grep "gaze-agent-linux-$arch" SHA256SUMS | sha256sum -c
   ```

3. Create the `gaze` user, install the binary, and write the token file:

   ```sh
   useradd --system --home /nonexistent --shell /usr/sbin/nologin gaze
   install -m 0755 "gaze-agent-linux-$arch" /usr/local/bin/gaze-agent
   install -d -m 0750 -o gaze -g gaze /etc/gaze
   ```

   Then write the token file. Run this command on its own, not pasted with
   the lines above: `read` takes whatever arrives next on the terminal, and
   in a pasted block that is the following line.

   ```sh
   (umask 077; read -rsp 'Token: ' t && echo && printf '%s\n' "$t" > /etc/gaze/token) && chown gaze:gaze /etc/gaze/token
   ```

   At the `Token:` prompt, paste the token. `read -s` keeps it off the
   screen and out of your shell history. The agent refuses a token file
   that group or other can read.

4. Download the unit file from the same release, check it, and install it
   with your server's proxied `https` URL in place of the placeholder:

   ```sh
   curl -fsSL "$base/gaze-agent.service" -o gaze-agent.service
   grep "gaze-agent.service" SHA256SUMS | sha256sum -c
   server=https://gaze.example.net
   sed "s|https://gaze.example.net|$server|" gaze-agent.service > /etc/systemd/system/gaze-agent.service
   ```

   Use the proxied URL on the server's own machine too. The server
   container publishes no port, so `http://localhost` does not reach it.

5. Decide whether the agent collects container statistics. The comments in
   [contrib/gaze-agent.service](contrib/gaze-agent.service) explain both
   choices:

   - To collect them, uncomment `SupplementaryGroups=docker`. Membership of
     the `docker` group is root in practice.
   - To skip them, add `-containers=false` to `ExecStart`. Without either
     change, the agent cannot open the socket and records an error on
     every sample.

6. Optional: To let the server manage the agent, add `-allow-remote-config`
   or `-allow-remote-update`, or both, to `ExecStart`. Remote update also
   needs the binary in a directory the `gaze` user can write to; the end of
   the unit file gives the steps.

7. Run the agent in the foreground to confirm it reaches the server:

   ```sh
   sudo -u gaze /usr/local/bin/gaze-agent -server "$server" -token-file /etc/gaze/token
   ```

   The first report arrives after about a minute. When the host list shows
   the host reporting, press Ctrl+C.

8. Start the service:

   ```sh
   systemctl daemon-reload
   systemctl enable --now gaze-agent
   journalctl -u gaze-agent -f
   ```

9. Optional, on Debian and Ubuntu: install the apt hook so the host list
   shows pending updates. See
   [counting pending updates](#counting-pending-updates).

#### Counting pending updates

The agent never runs apt: it runs unprivileged and reads files only. To
count pending updates, install an apt hook. Apt runs the hook as root after
every successful package-list refresh and after every install, and the
hook writes two numbers to `/var/lib/gaze/updates` for the agent to read.
Each apt run costs one simulated upgrade, which takes seconds at most.

1. In the same root shell, download both files and check them:

   ```sh
   curl -fsSL "$base/gaze-apt-updates" -o gaze-apt-updates
   curl -fsSL "$base/gaze-apt-updates.conf" -o gaze-apt-updates.conf
   grep -E ' gaze-apt-updates(\.conf)?$' SHA256SUMS | sha256sum -c
   ```

2. Install them, then take the first count:

   ```sh
   install -m 0755 gaze-apt-updates /usr/local/sbin/
   install -m 0644 gaze-apt-updates.conf /etc/apt/apt.conf.d/90gaze-apt-updates.conf
   /usr/local/sbin/gaze-apt-updates
   cat /var/lib/gaze/updates
   ```

   Apt runs the script as root, so keep it root-owned and out of any
   directory the `gaze` user can write — not `/usr/local/lib/gaze`, which
   remote update makes writable.

3. To check the count, compare it with apt's own list. The numbers match
   when the package lists are fresh, except on Ubuntu while an update is
   phased: `apt list` includes it, and `apt upgrade` and the hook do not.

   ```sh
   apt list --upgradable 2>/dev/null | grep -c upgradable
   ```

The count is only as fresh as the last `apt update`. The daily apt timer
refreshes the lists when `unattended-upgrades` is configured; without it,
the host list marks a count older than three days as out of date. A host
without the hook shows a dash, never zero.

On Debian, the restart flag depends on `unattended-upgrades` too: its
kernel hook creates `/run/reboot-required`. Ubuntu creates the marker by
default.

## The server

`gaze-server` ingests and stores reports in SQLite: raw for 7 days, 5-minute
roll-ups for 90 days, hourly for 2 years, each keeping minimum and maximum
beside the mean so spikes survive aggregation. It serves a web front end
over them: a host list that tells a reporting host from a stale one from
one that has never reported, and per-host graphs — server-rendered SVG, no
JavaScript — with tables for filesystems, containers, and the busiest
processes. The host page hides the virtual devices as the dashboard does, and
says how many; the link that shows them keeps the time range you were on.
The host list reloads itself every minute and prints the time it was read;
to see new data on a host page, reload the page.

Each row of the host list also shows uptime, a **restart** flag when the
distribution says an installed upgrade needs a boot, memory and the fullest
filesystem coloured by the same thresholds the TUI and the alert rules
use, and the pending update count from the
[apt hook](#counting-pending-updates). Hover over the disk figure to see
every filesystem. The server can also serve the TUI over SSH, which updates
live. See [The SSH view](#the-ssh-view).

It deploys as a container behind a TLS-terminating proxy and is never a
release asset. The image builds from source on the server, so getting the
software there is a clone of this repository.

### First-time setup

1. Clone the repository and mint the sealing key:

   ```
   git clone https://github.com/hammondus/gaze.git
   cd gaze
   echo "GAZE_KEY=$(head -c 32 /dev/urandom | base64)" >> .env
   ```

   `GAZE_KEY` seals the admin's TOTP secret; `.env` is git-ignored, and the
   key must stay out of the repository and away from the database.

   To print clock times in your own time zone rather than UTC, add its
   IANA name to the same file:

   ```
   echo "TZ=Australia/Queensland" >> .env
   ```

2. Wire the container to your proxy. `compose.yml` is deliberately
   unreachable as checked in — no published port, no shared network —
   because that wiring is deployment-specific. Put it in
   `compose.override.yml` beside `compose.yml`; the file is git-ignored,
   and `docker compose` merges it automatically. To join the Docker
   network your proxy is on:

   ```yaml
   services:
     gaze-server:
       networks:
         - proxynet          # the proxy's existing network
   networks:
     proxynet:
       external: true
   ```

3. Build and start the container:

   ```
   docker compose up -d --build
   make logs
   ```

4. Point the TLS-terminating proxy at `gaze-server:8080` over that shared
   network. The proxy is not optional: agents refuse plain `http` off
   loopback and the web session cookies are marked `Secure`, so nothing
   works end to end without the proxied `https` hostname.

5. On first start the log prints a setup code. Open `/setup` on the proxied
   URL, enter it, and create the admin account — a password and a mandatory
   authenticator enrolment.

6. Enroll the first host and start its agent. See
   [installing the agent](#installing-the-agent). The machine running the
   server wants an agent too, or it is the one host the fleet page cannot
   see.

Every later update is `make deploy` on the server: `git pull`, then
`docker compose up -d --build`. Alert mail can wait until the rules have
been watched for a while — with the `GAZE_SMTP_*` variables unset, alerts
compose into the log instead of sending.

If the admin is ever locked out, reset from the shell and set up again:

```
docker compose exec gaze-server gaze-server admin reset -db /data/gaze.db
```

### Enrolling a host

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

The server sends update directives only while it runs the latest release,
so an agent can never be pushed past its server's schema. The server reads
its version from `git describe` when `make deploy` builds the image, so
deploy with `make deploy`, from a checkout on the latest tag. A bare
`docker compose up --build` builds a `dev` server, and a checkout past the
tag builds a version such as `v0.6.0-1-gabc1234`; neither sends updates.
The first line of `make logs` names the running version, and while an
update stands requested, the log says why it is held.

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
make release  # build dist/gaze-linux-{arm64,amd64} and SHA256SUMS
make publish  # create a GitHub release for the current tag and attach the artifacts
```

### Cutting a release

```sh
git tag -a v0.4.0 -m "What changed since the last tag"
git push origin master --tags
make clean && make publish
```

Tag before building: the binaries embed their version at build time
from `git describe --tags`, and `--check-update` compares that embedded
version against the latest release, so a binary built before the tag
reports itself out of date forever. The `make clean` is what enforces
the order — the release targets rebuild only when a `.go` file is newer
than the existing `dist/` binaries, and tagging touches no `.go` file,
so without it a publish can attach binaries built before the tag with
the wrong version inside.

`make publish` needs the `gh` CLI, a clean working tree, and a tag on
`HEAD`; it refuses to run otherwise. It rebuilds the artifacts, then
creates the release with generated notes. The asset names are a
compatibility contract — every installed gaze fetches
`gaze-linux-<arch>` and `SHA256SUMS` by name — so a release must always
carry them.

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
| `internal/devices` | Which interfaces and block devices are virtual. Standard library only. |
| `internal/alert` | Threshold rules, staleness, and mail on transitions |
| `internal/threshold` | The warning and critical points the TUI, the web pages, and the alerts share. Standard library only. |
| `internal/ui` | Bubble Tea model, panels, and formatting |
| `internal/update` | `--update` and `--check-update` |

The shared type is `metrics.Snapshot`: the collector decides what the
numbers are, the display decides what they look like, and `report` reduces
a run of snapshots to what goes over the wire.

## Dev
For quick testing

in .zshrc add
```
export GAZE_PUSH_HOST=myhostname
```
```bash
make push
ssh -t myhostname ./gaze


## Licence

MIT. See [LICENSE](LICENSE).
