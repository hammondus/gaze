package report

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/metrics"
)

// demoSamples is a plausible busy machine observed six times, ten seconds
// apart, with fixed values throughout so the golden fixture is stable. The
// CPU spike in sample three is the point of the whole design: it must
// survive into Max after aggregation.
func demoSamples() []metrics.Snapshot {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	busy := []float64{12.5, 18.0, 96.0, 31.5, 22.0, 17.0}

	var samples []metrics.Snapshot
	for i := range busy {
		s := metrics.Snapshot{
			Taken:    base.Add(time.Duration(i) * 10 * time.Second),
			Interval: 10 * time.Second,
			Host: metrics.Host{
				Hostname: "web-01",
				Kernel:   "6.8.0-45-generic",
				CPUCount: 4,
				Uptime:   91*24*time.Hour + time.Duration(i)*10*time.Second,
			},
			CPU: metrics.CPU{Busy: busy[i]},
			Load: metrics.Load{
				One: 1.0 + float64(i)*0.1, Five: 0.8, Fifteen: 0.6,
			},
			Memory: metrics.Memory{
				Total: 8 << 30, Used: uint64(5<<30 + i*(100<<20)),
			},
			Swap: metrics.Swap{Total: 4 << 30, Used: 1 << 30},
			Networks: []metrics.Network{
				{Name: "eth0", RxRate: float64(i) * (1 << 20), TxRate: 300 << 10, Up: true},
				{Name: "wg0", RxRate: 20 << 10, TxRate: 15 << 10, Up: true},
				{Name: "lo", RxRate: 1 << 10, TxRate: 1 << 10, Up: true},
			},
			Disks: []metrics.Disk{
				{Name: "nvme0n1", ReadRate: 4 << 20, WriteRate: float64(i) * (2 << 20), IOPSRead: 120, IOPSWrite: 80},
				{Name: "sda", ReadRate: 0, WriteRate: 512 << 10, IOPSRead: 0, IOPSWrite: 12},
			},
			Mounts: []metrics.Mount{
				{Device: "/dev/nvme0n1p2", Path: "/", FSType: "ext4", Total: 250 << 30, Used: 180 << 30, Percent: 76.4},
				{Device: "/dev/nvme0n1p1", Path: "/boot", FSType: "vfat", Total: 1 << 30, Used: 200 << 20, Percent: 19.5},
				{Device: "/dev/sda1", Path: "/data", FSType: "xfs", Total: 4 << 40, Used: 3 << 40, Percent: 75.0},
				{Device: "tank/backup", Path: "/backup", FSType: "zfs", Total: 8 << 40, Used: 2 << 40, Percent: 25.0},
				{Device: "192.168.1.9:/media", Path: "/mnt/media", FSType: "nfs4", Total: 12 << 40, Used: 9 << 40, Percent: 75.0},
			},
			Processes: []metrics.Process{
				{PID: 2841, Name: "postgres", User: "postgres", Cmdline: "postgres: writer process", CPU: 18.2, RSS: 2 << 30},
				{PID: 1102, Name: "firefox", User: "craig", Cmdline: "/usr/lib/firefox/firefox", CPU: 11.7, RSS: 1500 << 20, Swap: 340 << 20},
				{PID: 7734, Name: "go", User: "craig", Cmdline: "go build ./...", CPU: 1.4, RSS: 350 << 20},
				{PID: 331, Name: "systemd-journald", User: "root", Cmdline: "/usr/lib/systemd/systemd-journald", CPU: 2.2, RSS: 68 << 20, Swap: 12 << 20},
				{PID: 412, Name: "java", User: "app", Cmdline: "java -Xmx4g -jar server.jar", CPU: 0.1, RSS: 3 << 30},
				{PID: 9021, Name: "kworker/2:1", User: "root", Kernel: true},
				{PID: 55, Name: "idle-thing", User: "nobody", CPU: 0, RSS: 4 << 20},
			},
			ProcCounts: metrics.ProcCounts{
				Total: 412, Running: 3, Sleeping: 400, Zombie: 1, Threads: 1183, Kernel: 180,
			},
			ContainerRuntime: "docker",
			Containers: []metrics.Container{
				{Name: "web", Image: "nginx:1.27", State: "running", CPU: 2.0 + float64(i), MemUsed: 120 << 20, MemLimit: 512 << 20},
				{Name: "db", Image: "postgres:16", State: "running", CPU: 8.5, MemUsed: 900 << 20, MemLimit: 2 << 30},
				{Name: "cache", Image: "redis:7", State: "running", CPU: 0.4, MemUsed: 60 << 20, MemLimit: 256 << 20},
				{Name: "queue", Image: "rabbitmq:3.13", State: "running", CPU: 1.1, MemUsed: 200 << 20, MemLimit: 1 << 30},
				{Name: "metrics", Image: "prom/prometheus:v2.53", State: "running", CPU: 0.9, MemUsed: 400 << 20, MemLimit: 1 << 30},
				{Name: "nightly-backup", Image: "restic/restic:0.17", State: "exited"},
				{Name: "migrate-once", Image: "flyway/flyway:10", State: "exited"},
			},
		}
		samples = append(samples, s)
	}
	return samples
}

// TestFromAggregates pins the aggregation arithmetic: the envelope and the
// mean, and the spike surviving into Max.
func TestFromAggregates(t *testing.T) {
	r := From(demoSamples(), Options{})

	if r.Schema != Schema {
		t.Errorf("schema = %d, want %d", r.Schema, Schema)
	}
	if r.Samples != 6 {
		t.Errorf("samples = %d, want 6", r.Samples)
	}
	if want := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC); !r.Start.Equal(want) {
		t.Errorf("start = %v, want %v", r.Start, want)
	}
	if want := time.Date(2026, 8, 25, 12, 0, 50, 0, time.UTC); !r.End.Equal(want) {
		t.Errorf("end = %v, want %v", r.End, want)
	}

	if r.CPU.Min != 12.5 || r.CPU.Max != 96.0 {
		t.Errorf("cpu envelope = %+v, want min 12.5 max 96", r.CPU)
	}
	if want := round2((12.5 + 18.0 + 96.0 + 31.5 + 22.0 + 17.0) / 6); r.CPU.Mean != want {
		t.Errorf("cpu mean = %v, want %v", r.CPU.Mean, want)
	}
	if r.Memory.Total != 8<<30 {
		t.Errorf("memory total = %d, want %d", r.Memory.Total, uint64(8<<30))
	}
	// Memory grows 100M per sample, so the envelope must not be flat.
	if r.Memory.Used.Min == r.Memory.Used.Max {
		t.Error("memory envelope is flat; aggregation is not seeing the series")
	}
}

// TestFromRanksProcesses covers the top-process selection: the busiest by
// CPU, the largest by memory, deduplicated, and nothing else.
func TestFromRanksProcesses(t *testing.T) {
	r := From(demoSamples(), Options{TopProcesses: 3})

	got := map[string]bool{}
	for _, p := range r.Top {
		got[p.Name] = true
	}
	// Top 3 by CPU: postgres, firefox, systemd-journald. Top 3 by RSS adds
	// java (3G); postgres and firefox are already there.
	for _, want := range []string{"postgres", "firefox", "systemd-journald", "java"} {
		if !got[want] {
			t.Errorf("top processes miss %s: %v", want, got)
		}
	}
	if len(r.Top) != 4 {
		t.Errorf("top processes = %d entries, want 4 after dedup", len(r.Top))
	}
	if r.Top[0].Name != "postgres" {
		t.Errorf("busiest = %s, want postgres", r.Top[0].Name)
	}
}

// TestFromZeroPadsMissing covers a process present in only some samples: the
// samples it missed count as zero, so its mean is diluted and its min is
// zero rather than the smallest value it happened to show.
func TestFromZeroPadsMissing(t *testing.T) {
	samples := demoSamples()
	// A short-lived process, present only in the last two samples.
	for i := 4; i < 6; i++ {
		samples[i].Processes = append(samples[i].Processes, metrics.Process{
			PID: 9999, Name: "burst", User: "craig", CPU: 60, RSS: 10 << 20,
		})
	}
	r := From(samples, Options{TopProcesses: 2})

	var burst *Process
	for i := range r.Top {
		if r.Top[i].Name == "burst" {
			burst = &r.Top[i]
		}
	}
	if burst == nil {
		t.Fatalf("burst process missing from top: %+v", r.Top)
	}
	if burst.CPU.Min != 0 {
		t.Errorf("burst min = %v, want 0 for the samples it missed", burst.CPU.Min)
	}
	if burst.CPU.Max != 60 {
		t.Errorf("burst max = %v, want 60", burst.CPU.Max)
	}
	if want := round2(120.0 / 6); burst.CPU.Mean != want {
		t.Errorf("burst mean = %v, want %v over all six samples", burst.CPU.Mean, want)
	}
}

// TestCmdlinesAreOptIn covers the default: no command line leaves the host
// unless the operator opted in.
func TestCmdlinesAreOptIn(t *testing.T) {
	for _, p := range From(demoSamples(), Options{}).Top {
		if p.Cmdline != "" {
			t.Errorf("%s carries a command line without the opt-in", p.Name)
		}
	}
	r := From(demoSamples(), Options{Cmdlines: true})
	var found bool
	for _, p := range r.Top {
		if p.Cmdline != "" {
			found = true
		}
	}
	if !found {
		t.Error("no command line present with Cmdlines: true")
	}
}

// TestFromCarriesAbsent covers the union and the string conversion: what the
// platform could not supply must reach the server, or a stored zero lies.
func TestFromCarriesAbsent(t *testing.T) {
	samples := demoSamples()
	samples[0].Absent = []metrics.Field{metrics.FieldSensors}
	samples[3].Absent = []metrics.Field{metrics.FieldProcessSwap, metrics.FieldSensors}
	r := From(samples, Options{})
	want := []string{"process.swap", "sensors"}
	if !reflect.DeepEqual(r.Absent, want) {
		t.Errorf("absent = %v, want %v", r.Absent, want)
	}
}

// TestFromEmpty pins the contract for no samples.
func TestFromEmpty(t *testing.T) {
	if r := From(nil, Options{}); !reflect.DeepEqual(r, Report{}) {
		t.Errorf("From(nil) = %+v, want the zero Report", r)
	}
}

// TestRoundTrip proves a Report survives its own wire format.
func TestRoundTrip(t *testing.T) {
	r := From(demoSamples(), Options{Cmdlines: true})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, back) {
		t.Errorf("round trip changed the report:\n got %+v\nwant %+v", back, r)
	}
}

// TestDirectiveRoundTrip proves the tri-state Containers field survives:
// nil, true, and false are three different instructions.
func TestDirectiveRoundTrip(t *testing.T) {
	off := false
	for _, d := range []Directive{
		{Generation: 3},
		{Generation: 4, SampleSeconds: 10, ReportSeconds: 60, Update: true},
		{Generation: 5, Containers: &off},
	} {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var back Directive
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d, back) {
			t.Errorf("round trip changed the directive: got %+v, want %+v", back, d)
		}
	}
}

// TestReportSize enforces the sizing decision the whole wire format exists
// for: the demo report stays under 4 KB of JSON.
func TestReportSize(t *testing.T) {
	b, err := json.Marshal(From(demoSamples(), Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 4<<10 {
		t.Errorf("report is %d bytes of JSON, the budget is %d", len(b), 4<<10)
	}
	t.Logf("report is %d bytes of JSON", len(b))
}

// TestGolden compares the marshalled demo report against the committed
// fixture, byte for byte. A failure here means the wire format changed:
// renaming or removing a field breaks every installed agent and every stored
// row, so either revert the change or bump Schema and update the fixture
// deliberately with GAZE_UPDATE_GOLDEN=1.
func TestGolden(t *testing.T) {
	b, err := json.MarshalIndent(From(demoSamples(), Options{Cmdlines: true}), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')

	const path = "testdata/report.json"
	if os.Getenv("GAZE_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run GAZE_UPDATE_GOLDEN=1 go test ./internal/report to create it", err)
	}
	if string(b) != string(want) {
		t.Errorf("the wire format changed; diff against %s:\n got: %s", path, b)
	}
}
