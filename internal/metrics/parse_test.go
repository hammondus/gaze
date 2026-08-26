package metrics

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// testSource points the collectors at the fixture tree, which is why every
// parser takes a filesystem rather than a path. These tests run on any
// platform, including the macOS machine this is developed on.
func testSource(t *testing.T) Source {
	t.Helper()
	return Source{
		Proc: os.DirFS("testdata/proc"),
		Sys:  os.DirFS("testdata/sys"),
	}
}

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// closeEnough compares floats to two decimal places, which is finer than
// anything this program displays.
func closeEnough(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestParseStat(t *testing.T) {
	s, err := parseStat(openFixture(t, "proc/stat"))
	if err != nil {
		t.Fatal(err)
	}
	if s.total.user != 1230000 || s.total.idle != 98000000 {
		t.Errorf("total ticks = %+v", s.total)
	}
	if len(s.perCPU) != 2 {
		t.Fatalf("per-CPU lines = %d, want 2", len(s.perCPU))
	}
	if s.perCPU[1].name != "cpu1" {
		t.Errorf("second core = %q, want cpu1", s.perCPU[1].name)
	}
	if s.running != 3 || s.blocked != 1 {
		t.Errorf("running/blocked = %d/%d, want 3/1", s.running, s.blocked)
	}
	// Guest time is already inside user, so total must not add it again.
	want := uint64(1230000 + 4500 + 320000 + 98000000 + 12000 + 0 + 8500 + 1200)
	if got := s.total.total(); got != want {
		t.Errorf("total() = %d, want %d", got, want)
	}
}

func TestCPUDelta(t *testing.T) {
	a, _ := parseStat(openFixture(t, "proc/stat"))
	b, _ := parseStat(openFixture(t, "proc/stat2"))

	c := cpuDelta(a.total, b.total)
	// The interval added 100 user, 50 system, 750 idle, 10 iowait and 90
	// softirq ticks, which is 1000 in total.
	if !closeEnough(c.User, 10) {
		t.Errorf("user = %.3f, want 10", c.User)
	}
	if !closeEnough(c.System, 5) {
		t.Errorf("system = %.3f, want 5", c.System)
	}
	if !closeEnough(c.Idle, 75) {
		t.Errorf("idle = %.3f, want 75", c.Idle)
	}
	if !closeEnough(c.Busy, 25) {
		t.Errorf("busy = %.3f, want 25", c.Busy)
	}
}

// A CPU that goes offline and returns brings reset counters with it. That must
// read as zero, never as a negative or a huge spike.
func TestCPUDeltaCounterReset(t *testing.T) {
	prev := cpuTicks{name: "cpu0", user: 900, idle: 900}
	cur := cpuTicks{name: "cpu0", user: 10, idle: 10}
	if c := cpuDelta(prev, cur); c.User != 0 || c.Busy != 0 {
		t.Errorf("reset produced %+v, want zeroes", c)
	}
}

func TestParseMeminfo(t *testing.T) {
	m, s, err := parseMeminfo(openFixture(t, "proc/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 16316536<<10 {
		t.Errorf("total = %d", m.Total)
	}
	// Used is total minus available, not total minus free.
	if want := uint64((16316536 - 10485760) << 10); m.Used != want {
		t.Errorf("used = %d, want %d", m.Used, want)
	}
	if !closeEnough(m.Percent, 35.7357) {
		t.Errorf("mem percent = %.4f, want 35.7357", m.Percent)
	}
	if want := uint64((4194304 - 4063232) << 10); s.Used != want {
		t.Errorf("swap used = %d, want %d", s.Used, want)
	}
	if m.Dirty != 153796<<10 || m.Writeback != 1024<<10 {
		t.Errorf("dirty = %d, writeback = %d", m.Dirty, m.Writeback)
	}
}

// Kernels before 3.14 have no MemAvailable line, and the fallback must still
// produce a sane figure.
func TestParseMeminfoWithoutMemAvailable(t *testing.T) {
	in := "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 200 kB\nSReclaimable: 50 kB\n"
	m, _, err := parseMeminfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.Available != 400<<10 {
		t.Errorf("available = %d, want %d", m.Available, 400<<10)
	}
	if m.Used != 600<<10 {
		t.Errorf("used = %d, want %d", m.Used, 600<<10)
	}
}

func TestParseLoadavg(t *testing.T) {
	l, err := parseLoadavg(openFixture(t, "proc/loadavg"))
	if err != nil {
		t.Fatal(err)
	}
	if l.One != 0.42 || l.Fifteen != 0.28 {
		t.Errorf("load = %+v", l)
	}
	if l.Runnable != 3 || l.Total != 1183 {
		t.Errorf("runnable/total = %d/%d, want 3/1183", l.Runnable, l.Total)
	}
}

func TestParseUptime(t *testing.T) {
	d, err := parseUptime(openFixture(t, "proc/uptime"))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Duration(987654.32 * float64(time.Second)); d != want {
		t.Errorf("uptime = %v, want %v", d, want)
	}
}

func TestParseNetDev(t *testing.T) {
	n, err := parseNetDev(openFixture(t, "proc/net/dev"))
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 3 {
		t.Fatalf("interfaces = %d, want 3", len(n))
	}
	if n["eth0"].rxBytes != 987654321 || n["eth0"].txBytes != 123456789 {
		t.Errorf("eth0 = %+v", n["eth0"])
	}
	// This name runs up against the colon with no padding, which is what
	// breaks a parser that splits on whitespace.
	if n["enp0s31f6"].rxBytes != 111 {
		t.Errorf("enp0s31f6 = %+v", n["enp0s31f6"])
	}
}

func TestParseDiskstats(t *testing.T) {
	d, err := parseDiskstats(openFixture(t, "proc/diskstats"))
	if err != nil {
		t.Fatal(err)
	}
	if d["nvme0n1"].readSectors != 41876480 {
		t.Errorf("nvme0n1 = %+v", d["nvme0n1"])
	}
	if _, ok := d["nvme0n1p1"]; !ok {
		t.Error("partitions must be parsed; the collector filters them, not the parser")
	}
}

func TestParseMounts(t *testing.T) {
	entries, err := parseMounts(openFixture(t, "proc/self/mounts"))
	if err != nil {
		t.Fatal(err)
	}
	var backup *mountEntry
	for i := range entries {
		if entries[i].device == "/dev/sda1" {
			backup = &entries[i]
		}
	}
	if backup == nil {
		t.Fatal("missing /dev/sda1")
	}
	// The kernel escapes the space as \040.
	if backup.path != "/mnt/backup drive" {
		t.Errorf("path = %q, want %q", backup.path, "/mnt/backup drive")
	}
}

func TestKeepMount(t *testing.T) {
	nodev, err := parseFilesystems(openFixture(t, "proc/filesystems"))
	if err != nil {
		t.Fatal(err)
	}
	if !nodev["proc"] || nodev["ext4"] {
		t.Fatalf("nodev set is wrong: %v", nodev)
	}

	entries, _ := parseMounts(openFixture(t, "proc/self/mounts"))
	seen := make(map[string]bool)
	var kept []string
	for _, m := range entries {
		if keepMount(m, nodev, seen) {
			kept = append(kept, m.path)
		}
	}
	// /dev/shm and /run are tmpfs under hidden prefixes: they are RAM, not
	// disk, and reporting them as filesystems is misleading.
	want := []string{"/", "/boot/efi", "/mnt/backup drive"}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i], want[i])
		}
	}
}

// /home is the same device as /, mounted twice. Showing both would report the
// same disk's usage as if it were two disks.
func TestKeepMountDropsDuplicateDevice(t *testing.T) {
	entries, _ := parseMounts(openFixture(t, "proc/self/mounts"))
	seen := make(map[string]bool)
	for _, m := range entries {
		keepMount(m, map[string]bool{}, seen)
	}
	for _, m := range entries {
		if m.path == "/home" && keepMount(m, map[string]bool{}, seen) {
			t.Error("/home shares a device with / and must not appear twice")
		}
	}
}

func TestParseProcStat(t *testing.T) {
	b, err := os.ReadFile("testdata/proc/842/stat")
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseProcStat(b)
	if err != nil {
		t.Fatal(err)
	}
	// The name holds a space and a closing bracket. The kernel does not
	// escape it, so the parser must scan to the last bracket.
	if p.comm != "my app (beta)" {
		t.Errorf("comm = %q, want %q", p.comm, "my app (beta)")
	}
	if p.pid != 842 || p.ppid != 1 || p.state != 'R' {
		t.Errorf("pid/ppid/state = %d/%d/%c", p.pid, p.ppid, p.state)
	}
	if p.utime != 8400 || p.stime != 1600 || p.ticks() != 10000 {
		t.Errorf("times = %d/%d", p.utime, p.stime)
	}
	if p.numThreads != 8 || p.starttime != 900 || p.rssPages != 65536 {
		t.Errorf("threads/start/rss = %d/%d/%d", p.numThreads, p.starttime, p.rssPages)
	}
}

// TestParseProcStatKernelThread checks the PF_KTHREAD flag, which is how a
// kernel thread is identified. The alternatives are guesses: an empty command
// line also describes a zombie, and a parent of PID 2 misses a kernel thread
// whose parent has exited.
func TestParseProcStatKernelThread(t *testing.T) {
	b, err := os.ReadFile("testdata/proc/9021/stat")
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseProcStat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !p.kernel() {
		t.Errorf("kworker flags = %#x, PF_KTHREAD not detected", p.flags)
	}

	// A user-space process must not be mistaken for one.
	b, _ = os.ReadFile("testdata/proc/842/stat")
	p, _ = parseProcStat(b)
	if p.kernel() {
		t.Errorf("user process flags = %#x, wrongly read as a kernel thread", p.flags)
	}

	// A zombie has no command line either, which is why an empty command line
	// is the wrong test.
	zombie := []byte("412 (defunct-thing) Z 1 412 412 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 900 0 0 0\n")
	p, err = parseProcStat(zombie)
	if err != nil {
		t.Fatal(err)
	}
	if p.kernel() {
		t.Error("a zombie is not a kernel thread")
	}
}

func TestParseProcStatTruncated(t *testing.T) {
	if _, err := parseProcStat([]byte("842 (sh) S 1 2 3\n")); err == nil {
		t.Error("want an error for a short stat line")
	}
	if _, err := parseProcStat([]byte("garbage")); err == nil {
		t.Error("want an error for a line with no comm field")
	}
}

// TestParseProcStatus checks the two fields status is read for: the owning
// user, and how much of the process sits in swap.
func TestParseProcStatus(t *testing.T) {
	st, err := parseProcStatus(openFixture(t, "proc/842/status"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.hasUID || st.uid != 1000 {
		t.Errorf("uid = %d (present %v), want 1000", st.uid, st.hasUID)
	}
	// The file reports kibibytes despite the kB suffix.
	if want := uint64(348160 << 10); st.swap != want {
		t.Errorf("swap = %d, want %d", st.swap, want)
	}

	// A process holding nothing in swap.
	st, _ = parseProcStatus(openFixture(t, "proc/1/status"))
	if st.swap != 0 || st.uid != 0 {
		t.Errorf("systemd = %+v, want uid 0 and no swap", st)
	}

	// A kernel thread has no address space, so status carries no Vm lines at
	// all. That must read as zero swap, not as a parse failure.
	st, err = parseProcStatus(openFixture(t, "proc/9021/status"))
	if err != nil {
		t.Fatal(err)
	}
	if st.swap != 0 {
		t.Errorf("kernel thread swap = %d, want 0", st.swap)
	}
	if !st.hasUID {
		t.Error("kernel thread has no uid")
	}
}

// TestParseProcStatusWithoutSwap covers a kernel built without CONFIG_SWAP,
// which omits VmSwap from every process.
func TestParseProcStatusWithoutSwap(t *testing.T) {
	in := "Name:\tsh\nUid:\t501\t501\t501\t501\nVmRSS:\t 1000 kB\nThreads:\t1\n"
	st, err := parseProcStatus(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if st.swap != 0 {
		t.Errorf("swap = %d, want 0", st.swap)
	}
	if st.uid != 501 {
		t.Errorf("uid = %d, want 501", st.uid)
	}
}

func TestParseCmdline(t *testing.T) {
	b, _ := os.ReadFile("testdata/proc/1/cmdline")
	if got := parseCmdline(b); got != "systemd --switched-root --system" {
		t.Errorf("cmdline = %q", got)
	}
	// A kernel thread has an empty cmdline.
	b, _ = os.ReadFile("testdata/proc/9021/cmdline")
	if got := parseCmdline(b); got != "" {
		t.Errorf("kernel thread cmdline = %q, want empty", got)
	}
}

func TestScanPIDs(t *testing.T) {
	pids, err := scanPIDs(testSource(t).Proc)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 4 {
		t.Fatalf("pids = %v, want 4 entries", pids)
	}
	for _, p := range pids {
		if p == 0 {
			t.Errorf("non-numeric directory leaked into %v", pids)
		}
	}
}

func TestReadSensors(t *testing.T) {
	s, err := readSensors(testSource(t).Sys)
	if err != nil {
		t.Fatal(err)
	}
	var temps, fans, batts int
	byLabel := map[string]Sensor{}
	for _, x := range s {
		byLabel[x.Label] = x
		switch x.Kind {
		case SensorTemp:
			temps++
		case SensorFan:
			fans++
		case SensorBattery:
			batts++
		}
	}
	if temps != 3 || fans != 1 || batts != 1 {
		t.Errorf("temps/fans/batteries = %d/%d/%d, want 3/1/1", temps, fans, batts)
	}
	// Millidegrees become degrees.
	if pkg := byLabel["Package id 0"]; !closeEnough(pkg.Value, 47) || !closeEnough(pkg.Crit, 100) {
		t.Errorf("package sensor = %+v", pkg)
	}
	// An unlabelled channel falls back to the chip name and channel.
	if _, ok := byLabel["nct6798 temp1"]; !ok {
		t.Errorf("missing fallback label, got %v", byLabel)
	}
	// A mains adapter is not a battery.
	if _, ok := byLabel["AC"]; ok {
		t.Error("the mains adapter must not appear as a battery")
	}
	if bat := byLabel["BAT0 (discharging)"]; !closeEnough(bat.Value, 87) {
		t.Errorf("battery = %+v", bat)
	}
}

func TestRateHandlesCounterReset(t *testing.T) {
	if got := rate(100, 200, 2); !closeEnough(got, 50) {
		t.Errorf("rate = %.2f, want 50", got)
	}
	if got := rate(500, 100, 2); got != 0 {
		t.Errorf("counter reset gave %.2f, want 0", got)
	}
	if got := rate(100, 200, 0); got != 0 {
		t.Errorf("zero interval gave %.2f, want 0", got)
	}
}

func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{
		"/mnt/plain":         "/mnt/plain",
		`/mnt/a\040b`:        "/mnt/a b",
		`/mnt/tab\011here`:   "/mnt/tab\there",
		`/mnt/back\134slash`: `/mnt/back\slash`,
		`/mnt/trailing\`:     `/mnt/trailing\`,
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}
