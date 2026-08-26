package main

import (
	"context"
	"database/sql"
	"math"
	"os"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

// TestSeedDemo writes a database for walking the web pages by hand: a host
// reporting now (with a two-hour outage yesterday, so the graphs show a
// gap), a host that stopped an hour ago, and one that never reported —
// the three states the stage-5 done-when requires at a glance. Opt-in,
// like the synthetic week in internal/store:
//
//	GAZE_DEMO_DB=/tmp/demo.db go test ./cmd/gaze-server -run TestSeedDemo
//	GAZE_KEY=$(head -c 32 /dev/urandom | base64) go run ./cmd/gaze-server -db /tmp/demo.db -insecure-cookies
func TestSeedDemo(t *testing.T) {
	path := os.Getenv("GAZE_DEMO_DB")
	if path == "" {
		t.Skip("set GAZE_DEMO_DB to a path to write a demo database")
	}
	os.Remove(path)
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := s.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute)
	seedDemoHost(t, s, "web-01", now.Add(-48*time.Hour), now, true)
	seedDemoHost(t, s, "db-01", now.Add(-48*time.Hour), now.Add(-time.Hour), false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The store stamps last_seen_at with its own clock on insert, so the
	// stopped host is backdated by hand — demo-only surgery, not a path
	// the server has.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE hosts SET last_seen_at = ? WHERE name = 'db-01'`,
		now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %s", path)
}

func seedDemoHost(t *testing.T, s *store.Store, name string, from, to time.Time, outage bool) {
	t.Helper()
	ctx := context.Background()
	token, err := s.Enroll(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}

	var batch []report.Report
	flush := func() {
		if len(batch) > 0 {
			if _, err := s.InsertReports(ctx, id, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	for ts := from; ts.Before(to); ts = ts.Add(time.Minute) {
		if outage {
			sinceOut := ts.Sub(to.Add(-26 * time.Hour))
			if sinceOut > 0 && sinceOut < 2*time.Hour {
				continue
			}
		}
		batch = append(batch, demoReport(name, ts))
		if len(batch) == 10 {
			flush()
		}
	}
	flush()
}

func demoReport(name string, t time.Time) report.Report {
	phase := float64(t.Unix()) / 3600
	cpu := 25 + 20*math.Sin(phase) + 10*math.Sin(phase*7)
	if t.Minute() == 30 {
		cpu = 92 // an hourly spike the envelope should keep visible
	}
	load := cpu / 25
	memUsed := float64(uint64(3)<<30) + float64(uint64(1)<<30)*math.Sin(phase/3)
	stat := func(mean, spread float64) report.Stat {
		return report.Stat{Min: mean - spread, Max: mean + spread, Mean: mean}
	}
	rx := 200e3 + 150e3*math.Sin(phase*2)
	return report.Report{
		Schema:  report.Schema,
		Version: "v0.5.0",
		Host:    report.Host{Hostname: name, Kernel: "6.8.0-45-generic", CPUCount: 4, UptimeSeconds: int64(t.Unix() % 4_000_000)},
		Start:   t, End: t.Add(50 * time.Second), Samples: 6,
		CPU:    stat(cpu, 8),
		Load1:  stat(load, 0.3),
		Load5:  stat(load*0.9, 0.2),
		Load15: stat(load*0.8, 0.1),
		Memory: report.Gauge{Total: 8 << 30, Used: stat(memUsed, 200e6)},
		Swap:   report.Gauge{Total: 2 << 30, Used: stat(80e6, 10e6)},
		Procs:  report.ProcCounts{Total: 187, Running: 2, Sleeping: 180, Zombie: 1, Threads: 400, Kernel: 90},
		// The bridge, the veths, and the loop device are here so the host
		// page's device toggle has something to hide: this is a host running
		// containers, and a real one carries a graph for each of them.
		Networks: []report.Network{
			{Name: "eth0", Rx: stat(rx, 60e3), Tx: stat(rx/4, 20e3), Up: true},
			{Name: "wg0", Rx: stat(4e3, 2e3), Tx: stat(3e3, 1e3), Up: true},
			{Name: "docker0", Rx: stat(rx/2, 30e3), Tx: stat(rx/8, 10e3), Up: true},
			{Name: "veth3a91c07", Rx: stat(rx/3, 20e3), Tx: stat(rx/9, 8e3), Up: true},
			{Name: "vethb52f8de", Rx: stat(rx/6, 10e3), Tx: stat(rx/12, 4e3), Up: true},
		},
		Disks: []report.Disk{
			{Name: "sda", Read: stat(400e3, 200e3), Write: stat(900e3, 300e3),
				ReadOps: stat(40, 15), WriteOps: stat(80, 20)},
			{Name: "loop0", Read: stat(8e3, 4e3), ReadOps: stat(2, 1)},
		},
		Mounts: []report.Mount{
			{Device: "/dev/sda1", Path: "/", FSType: "ext4", Total: 100 << 30, Used: 63 << 30, Percent: 63},
			{Device: "/dev/sdb1", Path: "/data", FSType: "xfs", Total: 500 << 30, Used: 455 << 30, Percent: 91},
		},
		Containers: []report.Container{
			{Name: "nginx", Image: "nginx:1.27", State: "running", CPU: stat(2, 1), Mem: 90 << 20},
			{Name: "postgres", Image: "postgres:16", State: "running", CPU: stat(8, 4), Mem: 800 << 20, MemLimit: 2 << 30},
			{Name: "backup", Image: "restic/restic", State: "exited"},
		},
		Top: []report.Process{
			{PID: 812, Name: "postgres", User: "postgres", CPU: stat(9, 3), RSS: 780 << 20, Cmdline: "postgres -D /var/lib/postgresql/data"},
			{PID: 4310, Name: "nginx", User: "www-data", CPU: stat(2, 1), RSS: 88 << 20},
			{PID: 977, Name: "gaze-agent", User: "gaze", CPU: stat(0.4, 0.2), RSS: 22 << 20, Cmdline: "/usr/local/bin/gaze-agent -server https://gaze.example.net"},
		},
		ContainerRuntime: "docker",
	}
}
