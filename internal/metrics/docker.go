package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dockerAPIVersion is pinned rather than left to the daemon's default so an
// upgrade on the host cannot change the response shape underneath you. Every
// Docker daemon since 20.10 serves this version, and Podman serves it through
// its Docker compatibility layer.
const dockerAPIVersion = "v1.41"

// dockerConcurrency bounds the requests in flight to the daemon. Two are made
// per running container, so this is the number of containers being polled at
// any moment, not the number of requests.
const dockerConcurrency = 8

// errNoRuntime reports that no container runtime is reachable, which is the
// normal case on most machines and not a failure worth showing.
var errNoRuntime = errors.New("no container runtime")

// dockerClient talks to a container runtime over its Unix socket using nothing
// but net/http. The official SDK pulls in a large dependency tree for what
// amounts to three GET requests against a documented JSON API.
//
// Podman needs no separate client. It serves the same endpoints on its own
// socket through its Docker compatibility layer, so only the socket path and
// the name reported on screen differ.
type dockerClient struct {
	http    *http.Client
	runtime string // "docker" or "podman", shown in the panel title
	prev    map[string]containerSample
}

// containerSample holds the cumulative counters needed to derive rates.
type containerSample struct {
	cpuTotal uint64
	sysTotal uint64
	rx, tx   uint64
	rd, wr   uint64
	at       time.Time
}

// socketCandidate is one place a container runtime might be listening.
type socketCandidate struct {
	path    string
	runtime string
}

// socketCandidates returns the sockets to try, in order.
//
// The environment variables come first so an explicit setting always wins.
// Podman's rootless socket lives under the user's runtime directory and its
// rootful socket under /run.
//
// Podman needs no separate client: it serves these same endpoints through its
// Docker compatibility layer, so only the path and the name reported on screen
// differ. That claim is not verified against a real Podman daemon here — see
// DESIGN-DECISIONS.md.
func socketCandidates() []socketCandidate {
	var out []socketCandidate
	add := func(path, runtime string) {
		if path != "" {
			out = append(out, socketCandidate{path, runtime})
		}
	}

	add(unixSocketFromEnv("DOCKER_HOST"), "docker")
	add(unixSocketFromEnv("CONTAINER_HOST"), "podman")
	add("/var/run/docker.sock", "docker")
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		add(filepath.Join(dir, "podman", "podman.sock"), "podman")
	}
	add("/run/podman/podman.sock", "podman")
	return out
}

// firstSocket returns the first candidate that is actually a socket.
//
// The mode is checked rather than mere existence, because a stale regular file
// at one of these paths would otherwise be chosen and every request against it
// would fail.
func firstSocket(cands []socketCandidate) (socketCandidate, bool) {
	for _, c := range cands {
		fi, err := os.Stat(c.path)
		if err == nil && fi.Mode()&os.ModeSocket != 0 {
			return c, true
		}
	}
	return socketCandidate{}, false
}

// unixSocketFromEnv returns the path in a unix:// URL, or an empty string for
// anything else. A TCP daemon is out of scope: this program reads a machine it
// is running on.
func unixSocketFromEnv(name string) string {
	v := os.Getenv(name)
	if path, ok := strings.CutPrefix(v, "unix://"); ok {
		return path
	}
	return ""
}

// newDockerClient returns a client for the first reachable runtime socket, or
// errNoRuntime if there is none.
func newDockerClient() (*dockerClient, error) {
	c, ok := firstSocket(socketCandidates())
	if !ok {
		return nil, errNoRuntime
	}
	sock := c.path
	return &dockerClient{
		runtime: c.runtime,
		http: &http.Client{
			Transport: &http.Transport{
				// Two requests are made per container per poll, so keeping
				// connections alive matters more here than the default of two
				// idle connections allows.
				MaxIdleConnsPerHost: dockerConcurrency,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		},
		prev: make(map[string]containerSample),
	}, nil
}

// get issues one GET against the daemon and decodes the JSON body into v.
// The host in the URL is ignored, since the transport always dials the socket.
func (c *dockerClient) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/"+dockerAPIVersion+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// Drain before closing, so the connection returns to the pool
		// instead of being torn down and redialled next poll.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", c.runtime, path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// dockerList is the subset of the container list response that is used.
type dockerList struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	Command string   `json:"Command"`
	Created int64    `json:"Created"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
}

// dockerInspect is the subset of the inspect response that is used.
type dockerInspect struct {
	State struct {
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
}

// blkioEntry is one row of the block I/O table, which reports a value per
// device per operation.
type blkioEntry struct {
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

// dockerStats is the subset of the stats response that is used.
type dockerStats struct {
	CPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System   uint64 `json:"system_cpu_usage"`
		OnlineCP uint64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			InactiveFile uint64 `json:"inactive_file"`
			Cache        uint64 `json:"cache"`
		} `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Blkio struct {
		ServiceBytes []blkioEntry `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	Pids struct {
		Current int `json:"current"`
	} `json:"pids_stats"`
}

// blkioTotals sums the per-device rows into read and write byte counts.
//
// The operation name is capitalised under cgroup v1 and lower case under v2,
// so the comparison folds case. Some storage drivers report nothing at all and
// leave the array null, which reads as zero.
func blkioTotals(entries []blkioEntry) (read, write uint64) {
	for _, e := range entries {
		switch strings.ToLower(e.Op) {
		case "read":
			read += e.Value
		case "write":
			write += e.Value
		}
	}
	return read, write
}

// collect returns one reading for every container, running or not.
//
// The list is fetched with all=1 so a stopped container can still be shown.
// Stats and inspect are only requested for running containers: a stopped one
// has no counters, and its exit time comes from the list response.
//
// Stats are fetched with one-shot=true and the rates derived across successive
// polls. Without one-shot, the daemon blocks for a second per call while it
// collects its own pair of samples, which would make the refresh interval a
// lie on a host running twenty containers.
func (c *dockerClient) collect(ctx context.Context) ([]Container, error) {
	var list []dockerList
	if err := c.get(ctx, "/containers/json?all=1", &list); err != nil {
		return nil, err
	}

	out := make([]Container, len(list))
	samples := make(map[string]containerSample, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, dockerConcurrency)
	now := time.Now()

	for i, l := range list {
		out[i] = Container{
			ID:      shortID(l.ID),
			Name:    strings.TrimPrefix(firstName(l.Names), "/"),
			Image:   l.Image,
			Command: l.Command,
			State:   l.State,
			Status:  l.Status,
			Created: time.Unix(l.Created, 0),
		}
		if l.State != "running" {
			continue
		}

		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Uptime needs State.StartedAt, which only the inspect endpoint
			// carries. Created is when the container was made, which is a
			// different and often much older moment. Measured against a live
			// daemon, inspect costs less than the stats call beside it, so it
			// is fetched every poll rather than cached: a cache would go stale
			// across a restart and report an uptime that never corrects.
			var ins dockerInspect
			if err := c.get(ctx, "/containers/"+id+"/json", &ins); err == nil {
				if t, err := time.Parse(time.RFC3339Nano, ins.State.StartedAt); err == nil && !t.IsZero() {
					mu.Lock()
					out[i].Started = t
					out[i].Uptime = now.Sub(t)
					mu.Unlock()
				}
			}

			var st dockerStats
			if err := c.get(ctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &st); err != nil {
				return
			}
			s := containerSample{
				cpuTotal: st.CPU.Usage.Total,
				sysTotal: st.CPU.System,
				at:       now,
			}
			for _, n := range st.Networks {
				s.rx += n.Rx
				s.tx += n.Tx
			}
			s.rd, s.wr = blkioTotals(st.Blkio.ServiceBytes)

			mu.Lock()
			prev, hadPrev := c.prev[id]
			samples[id] = s
			out[i].PIDs = st.Pids.Current
			out[i].MemUsed = containerMemory(st)
			out[i].MemPct = pct(out[i].MemUsed, st.Memory.Limit)
			out[i].MemLimit = st.Memory.Limit
			if hadPrev {
				out[i].CPU = containerCPU(prev, s, st.CPU.OnlineCP)
				if secs := s.at.Sub(prev.at).Seconds(); secs > 0 {
					out[i].RxRate = rate(prev.rx, s.rx, secs)
					out[i].TxRate = rate(prev.tx, s.tx, secs)
					out[i].ReadRate = rate(prev.rd, s.rd, secs)
					out[i].WriteRate = rate(prev.wr, s.wr, secs)
				}
			}
			mu.Unlock()
		}(i, l.ID)
	}
	wg.Wait()
	c.prev = samples
	return out, nil
}

// containerMemory returns memory in use excluding reclaimable page cache,
// matching what `docker stats` reports. The field name differs between cgroup
// v1 and v2, so whichever is present is subtracted.
func containerMemory(st dockerStats) uint64 {
	cache := st.Memory.Stats.InactiveFile
	if cache == 0 {
		cache = st.Memory.Stats.Cache
	}
	if cache > st.Memory.Usage {
		return 0
	}
	return st.Memory.Usage - cache
}

// containerCPU converts two cumulative CPU readings into a percentage of one
// core. Both counters are nanoseconds: the container's own CPU time, and the
// host's total across all cores.
func containerCPU(prev, cur containerSample, online uint64) float64 {
	if cur.cpuTotal < prev.cpuTotal || cur.sysTotal <= prev.sysTotal {
		return 0
	}
	if online == 0 {
		online = 1
	}
	cpuDelta := float64(cur.cpuTotal - prev.cpuTotal)
	sysDelta := float64(cur.sysTotal - prev.sysTotal)
	return cpuDelta / sysDelta * float64(online) * 100
}

// shortID trims a container ID to the twelve characters Docker itself shows.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// firstName returns the first of a container's names, or an empty string.
func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
