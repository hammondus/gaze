package metrics

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSocketCandidateOrder checks that an explicit setting wins over the
// defaults, and that a TCP daemon is ignored.
func TestSocketCandidateOrder(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/custom-docker.sock")
	t.Setenv("CONTAINER_HOST", "unix:///tmp/custom-podman.sock")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/501")

	got := socketCandidates()
	want := []socketCandidate{
		{"/tmp/custom-docker.sock", "docker"},
		{"/tmp/custom-podman.sock", "podman"},
		{"/var/run/docker.sock", "docker"},
		{"/run/user/501/podman/podman.sock", "podman"},
		{"/run/podman/podman.sock", "podman"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %v, want %v", i, got[i], want[i])
		}
	}

	// A remote daemon is out of scope: this program reads a machine it runs
	// on, so a tcp:// setting must be ignored rather than half-honoured.
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	for _, c := range socketCandidates() {
		if strings.Contains(c.path, "10.0.0.5") {
			t.Errorf("a tcp DOCKER_HOST leaked into the candidates: %v", c)
		}
	}
}

// TestFirstSocketPicksASocket checks the probe. Podman is not installed on the
// development machine, so this covers the discovery that would find it: the
// path, the runtime name, and the refusal to accept a regular file.
func TestFirstSocketPicksASocket(t *testing.T) {
	// Not t.TempDir: a Unix socket path is capped at 104 bytes on macOS and
	// 108 on Linux, and the test temp path alone exceeds that.
	dir, err := os.MkdirTemp("/tmp", "mg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	podmanDir := filepath.Join(dir, "podman")
	if err := os.MkdirAll(podmanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(podmanDir, "podman.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	defer l.Close()

	// A regular file at an earlier candidate must be skipped, not chosen. A
	// stale file would otherwise win and every request against it would fail.
	stale := filepath.Join(dir, "stale-docker.sock")
	if err := os.WriteFile(stale, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := firstSocket([]socketCandidate{
		{filepath.Join(dir, "missing.sock"), "docker"},
		{stale, "docker"},
		{sock, "podman"},
	})
	if !ok {
		t.Fatal("the podman socket was not found")
	}
	if got.path != sock || got.runtime != "podman" {
		t.Errorf("chose %v, want %s as podman", got, sock)
	}

	if _, ok := firstSocket([]socketCandidate{{stale, "docker"}}); ok {
		t.Error("a regular file was accepted as a socket")
	}
}

// TestBlkioTotals covers the block I/O table, whose operation names are
// capitalised under cgroup v1 and lower case under v2.
func TestBlkioTotals(t *testing.T) {
	r, w := blkioTotals([]blkioEntry{
		{Op: "read", Value: 4096},
		{Op: "write", Value: 8192},
		{Op: "Read", Value: 1024},
		{Op: "Write", Value: 2048},
		{Op: "sync", Value: 999999},  // not a direction; must be ignored
		{Op: "async", Value: 999999}, // likewise
		{Op: "total", Value: 999999}, // cgroup v1 emits this and it double-counts
	})
	if r != 5120 {
		t.Errorf("read = %d, want 5120", r)
	}
	if w != 10240 {
		t.Errorf("write = %d, want 10240", w)
	}
	// A storage driver that reports nothing leaves the array null.
	if r, w := blkioTotals(nil); r != 0 || w != 0 {
		t.Errorf("nil blkio = %d/%d, want 0/0", r, w)
	}
}

// TestContainerCPU covers the two-sample CPU calculation, including the
// counter reset that a restart produces.
func TestContainerCPU(t *testing.T) {
	// One core fully used: the container's CPU time grew by the same amount
	// as one core's share of the host total.
	prev := containerSample{cpuTotal: 1_000_000_000, sysTotal: 80_000_000_000}
	cur := containerSample{cpuTotal: 2_000_000_000, sysTotal: 88_000_000_000}
	if got := containerCPU(prev, cur, 8); got < 99.9 || got > 100.1 {
		t.Errorf("cpu = %.2f, want 100", got)
	}
	// A restart resets the container's counter.
	if got := containerCPU(cur, prev, 8); got != 0 {
		t.Errorf("counter reset gave %.2f, want 0", got)
	}
	// No host movement means no divisor.
	if got := containerCPU(prev, containerSample{cpuTotal: 2e9, sysTotal: prev.sysTotal}, 8); got != 0 {
		t.Errorf("zero host delta gave %.2f, want 0", got)
	}
	// A daemon that does not report core count must not divide by zero.
	if got := containerCPU(prev, cur, 0); got <= 0 {
		t.Errorf("missing core count gave %.2f, want a positive value", got)
	}
}

// TestContainerMemory checks the page cache is excluded, matching what
// `docker stats` reports, under both cgroup versions.
func TestContainerMemory(t *testing.T) {
	var st dockerStats
	st.Memory.Usage = 100 << 20
	st.Memory.Stats.InactiveFile = 30 << 20 // cgroup v2
	if got := containerMemory(st); got != 70<<20 {
		t.Errorf("cgroup v2 memory = %d, want %d", got, 70<<20)
	}

	st.Memory.Stats.InactiveFile = 0
	st.Memory.Stats.Cache = 40 << 20 // cgroup v1
	if got := containerMemory(st); got != 60<<20 {
		t.Errorf("cgroup v1 memory = %d, want %d", got, 60<<20)
	}

	// Cache larger than usage is nonsense, and must not underflow.
	st.Memory.Usage = 10 << 20
	if got := containerMemory(st); got != 0 {
		t.Errorf("cache above usage gave %d, want 0", got)
	}
}

// TestLiveContainers polls a real Docker daemon twice and checks the derived
// figures.
//
// Container CPU and network are rates, so they need two samples: the first
// poll reports zero and the second reports the truth. Nothing but a live
// daemon can cover this, so it is opt-in. To run it, start a container that
// spins a core and name it mg-busy:
//
//	docker run -d --rm --name mg-busy --memory=256m alpine sh -c 'while :; do :; done'
//	GAZE_LIVE=1 go test ./internal/metrics -run TestLiveContainers -v
func TestLiveContainers(t *testing.T) {
	if os.Getenv("GAZE_LIVE") == "" {
		t.Skip("set GAZE_LIVE=1 and start a container named mg-busy")
	}
	c := New()
	if c.docker == nil {
		t.Skip("no docker daemon reachable")
	}
	ctx := context.Background()

	// The first poll has nothing to diff against, so every rate must be zero
	// rather than a spike computed from a zero baseline.
	first := c.Collect(ctx)
	if len(first.Containers) == 0 {
		t.Skip("no containers running")
	}
	for _, x := range first.Containers {
		if x.CPU != 0 || x.RxRate != 0 || x.TxRate != 0 {
			t.Errorf("first poll of %s reported rates: %+v", x.Name, x)
		}
		if x.MemUsed == 0 && x.State == "running" {
			t.Errorf("%s reports no memory", x.Name)
		}
	}

	time.Sleep(1500 * time.Millisecond)
	second := c.Collect(ctx)

	var busy *Container
	for i := range second.Containers {
		if second.Containers[i].Name == "mg-busy" {
			busy = &second.Containers[i]
		}
	}
	if busy == nil {
		t.Skip("no container named mg-busy")
	}
	if busy.CPU < 50 {
		t.Errorf("mg-busy spins a core but reports %.1f%% CPU", busy.CPU)
	}
	if busy.MemPct <= 0 || busy.MemPct > 100 {
		t.Errorf("mg-busy memory = %.2f%% of its limit, want a sane fraction", busy.MemPct)
	}
}
