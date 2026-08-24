package metrics

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// loadMapFS copies a fixture directory into an in-memory filesystem, so a test
// can advance the counters between two collections the way a running kernel
// would.
//
// It resolves symlinked directories rather than skipping them, because
// /sys/class is built entirely out of symlinks into the device tree and the
// fixtures mirror that.
func loadMapFS(t *testing.T, root string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	var walk func(dir, prefix string)
	walk = func(dir, prefix string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			name := path.Join(prefix, e.Name())
			// Stat follows symlinks; the DirEntry type does not.
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if fi.IsDir() {
				walk(p, name)
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			m[name] = &fstest.MapFile{Data: b, Mode: 0o444}
		}
	}
	walk(root, "")
	return m
}

// TestCollectDeltas runs a full collection across two generations of counters
// and checks that the rates come out right. This is the path that carries all
// the risk: every number on screen is a difference between two readings.
func TestCollectDeltas(t *testing.T) {
	proc := loadMapFS(t, "testdata/proc")
	sys := loadMapFS(t, "testdata/sys")
	c := NewWithSource(Source{Proc: proc, Sys: sys})

	// Advance the machine by 1000 CPU jiffies across two cores, of which
	// PID 842 consumed 100.
	proc["stat"] = proc["stat2"]
	bump(t, proc, "842/stat", "8400 1600", "8480 1620")
	bump(t, proc, "net/dev", "eth0: 987654321", "eth0: 987664321")

	// The priming sample in NewWithSource was taken a moment ago, so the
	// interval is close to zero. Pin it to exactly two seconds to make the
	// rate arithmetic checkable.
	c.prevAt = c.prevAt.Add(-2000000000)

	s := c.Collect(context.Background())
	if len(s.Errs) > 0 {
		// statfs cannot run against fixture paths off Linux, so a mount
		// failure here is expected and not what this test is about.
		for _, err := range s.Errs {
			t.Logf("collector error: %v", err)
		}
	}

	if !closeEnough(s.CPU.Busy, 25) {
		t.Errorf("CPU busy = %.2f, want 25", s.CPU.Busy)
	}
	if len(s.PerCPU) != 2 {
		t.Errorf("per-CPU entries = %d, want 2", len(s.PerCPU))
	}

	// 100 of the machine's 1000 jiffies, over two cores, is 20 percent of one
	// core. The USER_HZ constant does not enter this: it cancels.
	var app *Process
	for i := range s.Processes {
		if s.Processes[i].PID == 842 {
			app = &s.Processes[i]
		}
	}
	if app == nil {
		t.Fatal("PID 842 missing from the process list")
	}
	if !closeEnough(app.CPU, 20) {
		t.Errorf("PID 842 CPU = %.2f, want 20", app.CPU)
	}
	if app.Name != "my app (beta)" {
		t.Errorf("PID 842 name = %q", app.Name)
	}
	if app.RSS != 65536*uint64(os.Getpagesize()) {
		t.Errorf("PID 842 RSS = %d", app.RSS)
	}

	// 10000 bytes over two seconds. The interval is wall-clock, so the
	// priming sample's own age shows up as a fraction of a percent.
	var eth *Network
	for i := range s.Networks {
		if s.Networks[i].Name == "eth0" {
			eth = &s.Networks[i]
		}
	}
	if eth == nil {
		t.Fatal("eth0 missing")
	}
	if eth.RxRate < 4999 || eth.RxRate > 5001 {
		t.Errorf("eth0 rx rate = %.2f, want about 5000", eth.RxRate)
	}
	if !eth.Up {
		t.Error("eth0 reports operstate up")
	}

	if s.ProcCounts.Total != 4 || s.ProcCounts.Running != 1 || s.ProcCounts.Sleeping != 3 {
		t.Errorf("process counts = %+v", s.ProcCounts)
	}
	if s.ProcCounts.Threads != 14 {
		t.Errorf("thread count = %d, want 14", s.ProcCounts.Threads)
	}

	// Swap and the owning user both come from /proc/<pid>/status.
	if app.Swap != 348160<<10 {
		t.Errorf("PID 842 swap = %d, want %d", app.Swap, uint64(348160)<<10)
	}
	if app.User == "" {
		t.Error("PID 842 has no owner")
	}
	for _, p := range s.Processes {
		if p.PID == 9021 && p.Swap != 0 {
			t.Errorf("kernel thread swap = %d, want 0", p.Swap)
		}
	}

	// One of the four fixture processes is a kworker.
	if s.ProcCounts.Kernel != 1 {
		t.Errorf("kernel threads = %d, want 1", s.ProcCounts.Kernel)
	}
	for _, p := range s.Processes {
		if want := p.PID == 9021; p.Kernel != want {
			t.Errorf("PID %d Kernel = %v, want %v", p.PID, p.Kernel, want)
		}
	}

	// /sys/class/hwmon is built out of symlinks into the device tree, and the
	// fixtures mirror that, so this also checks the scan resolves them.
	if len(s.Sensors) != 5 {
		t.Errorf("sensors = %d, want 5", len(s.Sensors))
	}
}

// TestFirstCollectionHasNoRates checks the frame drawn at start-up.
//
// New primes the counters and the display collects straight away, so the first
// interval is about a millisecond. Every rate is a difference over elapsed
// time, and over a millisecond one jiffy of CPU reads as hundreds of percent.
// The first frame must show zeroes, not nonsense.
func TestFirstCollectionHasNoRates(t *testing.T) {
	proc := loadMapFS(t, "testdata/proc")
	c := NewWithSource(Source{Proc: proc, Sys: loadMapFS(t, "testdata/sys")})

	// Advance every counter as if a busy second had passed, but collect
	// immediately, the way Init does.
	proc["stat"] = proc["stat2"]
	bump(t, proc, "842/stat", "8400 1600", "9400 1600")
	bump(t, proc, "net/dev", "eth0: 987654321", "eth0: 999654321")

	s := c.Collect(context.Background())

	if s.CPU.Busy != 0 {
		t.Errorf("first frame CPU busy = %.2f, want 0", s.CPU.Busy)
	}
	for _, p := range s.Processes {
		if p.CPU != 0 {
			t.Errorf("first frame PID %d CPU = %.2f, want 0", p.PID, p.CPU)
		}
	}
	for _, n := range s.Networks {
		if n.RxRate != 0 || n.TxRate != 0 {
			t.Errorf("first frame %s rates = %.2f/%.2f, want 0", n.Name, n.RxRate, n.TxRate)
		}
	}
	// Absolute readings are still valid: only the derived figures are held back.
	if s.Memory.Total == 0 || len(s.Processes) != 4 {
		t.Errorf("first frame lost its absolute values: mem=%d procs=%d",
			s.Memory.Total, len(s.Processes))
	}
	if len(s.PerCPU) != 2 {
		t.Errorf("first frame per-CPU entries = %d, want 2", len(s.PerCPU))
	}

	// The second collection diffs against the first and reports real figures.
	c.prevAt = c.prevAt.Add(-time.Second)
	bump(t, proc, "842/stat", "9400 1600", "9500 1600")
	proc["stat"] = &fstest.MapFile{
		Data: []byte("cpu  1230600 4500 320050 98000750 12010 0 8590 1200 0 0\n" +
			"cpu0 620060 2200 160020 49000350 6005 0 4340 600 0 0\n" +
			"cpu1 610040 2300 160030 49000400 6005 0 4250 600 0 0\n"),
		Mode: 0o444,
	}
	s2 := c.Collect(context.Background())
	if s2.CPU.Busy == 0 {
		t.Error("second frame still reports no CPU activity")
	}
}

// TestCollectFiltersPartitions checks that a partition's I/O is not counted
// alongside its parent device.
func TestCollectFiltersPartitions(t *testing.T) {
	c := NewWithSource(Source{
		Proc: loadMapFS(t, "testdata/proc"),
		Sys:  loadMapFS(t, "testdata/sys"),
	})
	s := c.Collect(context.Background())

	names := map[string]bool{}
	for _, d := range s.Disks {
		names[d.Name] = true
	}
	if !names["nvme0n1"] || !names["sda"] {
		t.Errorf("whole devices missing from %v", names)
	}
	if names["nvme0n1p1"] {
		t.Error("a partition was counted alongside its parent device")
	}
	if names["loop0"] {
		t.Error("loop0 is not in /sys/block and must be filtered")
	}
}

// TestCollectDropsUnusedDevices checks that block devices which have never
// done any I/O are left out. A container host carries two dozen idle loop and
// nbd devices, which is enough to bury the real disks.
func TestCollectDropsUnusedDevices(t *testing.T) {
	proc := loadMapFS(t, "testdata/proc")
	sys := loadMapFS(t, "testdata/sys")
	// nbd0 is a whole device in /sys/block with counters that have never moved.
	sys["block/nbd0/size"] = &fstest.MapFile{Data: []byte("0\n"), Mode: 0o444}
	proc["diskstats"] = &fstest.MapFile{
		Data: append(proc["diskstats"].Data,
			[]byte("  43       0 nbd0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n")...),
		Mode: 0o444,
	}

	s := NewWithSource(Source{Proc: proc, Sys: sys}).Collect(context.Background())
	for _, d := range s.Disks {
		if d.Name == "nbd0" {
			t.Error("nbd0 has never done any I/O and must not be listed")
		}
	}
	if len(s.Disks) != 2 {
		t.Errorf("disks = %d, want 2", len(s.Disks))
	}
}

// bump replaces a substring in a fixture file to simulate a counter advancing.
func bump(t *testing.T, m fstest.MapFS, name, old, new string) {
	t.Helper()
	f, ok := m[name]
	if !ok {
		t.Fatalf("fixture %s missing", name)
	}
	s := string(f.Data)
	if !strings.Contains(s, old) {
		t.Fatalf("fixture %s does not contain %q", name, old)
	}
	m[name] = &fstest.MapFile{Data: []byte(strings.Replace(s, old, new, 1)), Mode: 0o444}
}
