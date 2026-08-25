package query

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

// seed opens a store, enrolls a host, and returns both with the query side.
func seed(t *testing.T) (*store.Store, *Q, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token, err := s.Enroll(context.Background(), "web-01")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return s, New(s.Read()), id
}

func sampleReport(start time.Time) report.Report {
	return report.Report{
		Schema:  report.Schema,
		Version: "v1.0.0",
		Host:    report.Host{Hostname: "web-01", Kernel: "6.8.0", CPUCount: 4, UptimeSeconds: 500},
		Start:   start,
		End:     start.Add(50 * time.Second),
		Samples: 6,
		CPU:     report.Stat{Min: 10, Max: 90, Mean: 33.5},
		Load1:   report.Stat{Min: 1, Max: 2, Mean: 1.5},
		Memory:  report.Gauge{Total: 8 << 30, Used: report.Stat{Min: 1, Max: 3, Mean: 2}},
		Swap:    report.Gauge{Total: 4 << 30},
		Procs:   report.ProcCounts{Total: 200, Running: 3, Zombie: 1},
		Networks: []report.Network{
			{Name: "eth0", Rx: report.Stat{Min: 1, Max: 3, Mean: 2}, Up: true},
			{Name: "wg0", Up: true},
		},
		Disks:  []report.Disk{{Name: "sda", Read: report.Stat{Min: 1, Max: 2, Mean: 1.5}}},
		Mounts: []report.Mount{{Device: "/dev/sda1", Path: "/", FSType: "ext4", Total: 10, Used: 5, Percent: 50}},
		Containers: []report.Container{
			{Name: "web", Image: "nginx", State: "running", CPU: report.Stat{Min: 1, Max: 2, Mean: 1.5}, Mem: 9, MemLimit: 99},
		},
		Top: []report.Process{
			{PID: 9, Name: "postgres", User: "pg", CPU: report.Stat{Min: 5, Max: 9, Mean: 7}, RSS: 42, Swap: 1, Cmdline: "postgres -D /data"},
			{PID: 3, Name: "go", User: "craig", CPU: report.Stat{Min: 0, Max: 1, Mean: 0.5}, RSS: 7},
		},
		ContainerRuntime: "docker",
		Absent:           []string{"sensors"},
	}
}

// TestHosts covers the fleet list, including a host that has never
// reported.
func TestHosts(t *testing.T) {
	s, q, id := seed(t)
	ctx := context.Background()
	if _, err := s.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertReports(ctx, id, []report.Report{sampleReport(time.Now().Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}

	hosts, err := q.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(hosts))
	}
	// Ordered by name: silent-01 then web-01.
	if hosts[0].Name != "silent-01" || !hosts[0].LastSeen.IsZero() {
		t.Errorf("silent host = %+v, want zero LastSeen", hosts[0])
	}
	if hosts[1].Name != "web-01" || hosts[1].LastSeen.IsZero() || hosts[1].AgentVersion != "v1.0.0" {
		t.Errorf("reporting host = %+v", hosts[1])
	}
}

// TestScalars covers the range query on the raw tier.
func TestScalars(t *testing.T) {
	s, q, id := seed(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute)
	for i := range 5 {
		r := sampleReport(now.Add(time.Duration(i-10) * time.Minute))
		if _, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil {
			t.Fatal(err)
		}
	}

	points, err := q.Scalars(ctx, id, now.Add(-9*time.Minute), now.Add(-7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2 (the range is half-open)", len(points))
	}
	p := points[0]
	if p.CPU.Mean != 33.5 || p.MemTotal != 8<<30 || p.Zombies != 1 {
		t.Errorf("point = %+v", p)
	}
	if !p.Start.Equal(now.Add(-9 * time.Minute)) {
		t.Errorf("start = %v, want %v", p.Start, now.Add(-9*time.Minute))
	}
}

// TestLatest proves the reconstruction round-trips: what the agent posted
// is what presentation gets back, series and processes included.
func TestLatest(t *testing.T) {
	s, q, id := seed(t)
	ctx := context.Background()

	older := sampleReport(time.Now().Add(-2 * time.Minute).Truncate(time.Second))
	posted := sampleReport(time.Now().Add(-time.Minute).Truncate(time.Second))
	if _, err := s.InsertReports(ctx, id, []report.Report{older, posted}); err != nil {
		t.Fatal(err)
	}

	got, err := q.Latest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(posted.Start) {
		t.Fatalf("latest starts %v, want %v", got.Start, posted.Start)
	}
	if !reflect.DeepEqual(got.Networks, posted.Networks) {
		t.Errorf("networks: got %+v, want %+v", got.Networks, posted.Networks)
	}
	if !reflect.DeepEqual(got.Disks, posted.Disks) {
		t.Errorf("disks: got %+v, want %+v", got.Disks, posted.Disks)
	}
	if !reflect.DeepEqual(got.Mounts, posted.Mounts) {
		t.Errorf("mounts: got %+v, want %+v", got.Mounts, posted.Mounts)
	}
	if !reflect.DeepEqual(got.Containers, posted.Containers) {
		t.Errorf("containers: got %+v, want %+v", got.Containers, posted.Containers)
	}
	if !reflect.DeepEqual(got.Top, posted.Top) {
		t.Errorf("top: got %+v, want %+v", got.Top, posted.Top)
	}
	if !reflect.DeepEqual(got.Absent, posted.Absent) {
		t.Errorf("absent: got %v, want %v", got.Absent, posted.Absent)
	}
	if got.CPU != posted.CPU || got.Procs != posted.Procs {
		t.Errorf("scalars: got %+v / %+v", got.CPU, got.Procs)
	}
}
