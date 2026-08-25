package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
)

// TestSyntheticWeek is the stage 4 done-when condition: a week of reports
// for ten hosts, posted minute by minute against a steered clock, must roll
// up correctly and land within a factor of two of the size estimate in
// DESIGN-DECISIONS. It writes about two million rows, so it is opt-in:
//
//	GAZE_SYNTH=1 go test ./internal/store -run TestSyntheticWeek -v
func TestSyntheticWeek(t *testing.T) {
	if os.Getenv("GAZE_SYNTH") == "" {
		t.Skip("set GAZE_SYNTH=1 to write a synthetic week of data")
	}
	path := filepath.Join(t.TempDir(), "gaze.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	const hosts = 10
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sim := start
	s.now = func() time.Time { return sim }

	ids := make([]int64, hosts)
	for h := range ids {
		token, err := s.Enroll(ctx, fmt.Sprintf("host-%02d", h))
		if err != nil {
			t.Fatal(err)
		}
		if ids[h], err = s.Authenticate(ctx, token); err != nil {
			t.Fatal(err)
		}
	}

	const (
		minutes   = 7 * 24 * 60
		batchSize = 10
	)
	wall := time.Now()
	for m := 0; m < minutes; m += batchSize {
		for h, id := range ids {
			batch := make([]report.Report, 0, batchSize)
			for i := m; i < m+batchSize && i < minutes; i++ {
				batch = append(batch, synthReport(start.Add(time.Duration(i)*time.Minute), h, i))
			}
			sim = start.Add(time.Duration(m+batchSize) * time.Minute)
			if _, err := s.InsertReports(ctx, id, batch); err != nil {
				t.Fatal(err)
			}
		}
		if m%60 == 0 {
			if err := s.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	sim = start.Add(minutes * time.Minute)
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("ingested %d reports for %d hosts in %s", minutes*hosts, hosts, time.Since(wall).Round(time.Second))

	// Row counts must match the geometry exactly: every complete window
	// older than the two-hour hold is rolled, for every host.
	var until1, until2 int64
	s.read.QueryRow(`SELECT until FROM rollup_state WHERE tier = 1`).Scan(&until1)
	s.read.QueryRow(`SELECT until FROM rollup_state WHERE tier = 2`).Scan(&until2)
	wantUntil1 := (sim.Add(-rollDelay).Unix() / 300) * 300
	if until1 != wantUntil1 {
		t.Errorf("tier 1 watermark = %d, want %d", until1, wantUntil1)
	}
	for tier, want := range map[int]int64{
		1: hosts * (until1 - start.Unix()) / 300,
		2: hosts * (until2 - start.Unix()) / 3600,
	} {
		var got int64
		s.read.QueryRow(`SELECT count(*) FROM reports WHERE tier = ?`, tier).Scan(&got)
		if got != want {
			t.Errorf("tier %d has %d rows, want %d", tier, got, want)
		}
	}

	// The raw tier holds only the retention window.
	var raw int64
	s.read.QueryRow(`SELECT count(*) FROM reports WHERE tier = 0`).Scan(&raw)
	if perHost := raw / hosts; perHost < 7*24*60-120 || perHost > 7*24*60 {
		t.Errorf("raw tier holds %d rows per host, want about a week", perHost)
	}

	// One five-minute window, recomputed by hand: the weighted mean of five
	// equal-weight reports is their plain mean, and the envelope is the
	// widest of them.
	w0 := start.Add(24 * time.Hour)
	var mean, minv, maxv float64
	err = s.read.QueryRow(`
		SELECT cpu_mean, cpu_min, cpu_max FROM reports
		WHERE host_id = ? AND tier = 1 AND start = ?`, ids[3], w0.Unix()).
		Scan(&mean, &minv, &maxv)
	if err != nil {
		t.Fatalf("no tier 1 row at %v: %v", w0, err)
	}
	var wantMean, wantMin, wantMax float64
	wantMin, wantMax = 999, -1
	for i := 24 * 60; i < 24*60+5; i++ {
		r := synthReport(start.Add(time.Duration(i)*time.Minute), 3, i)
		wantMean += r.CPU.Mean / 5
		wantMin = min(wantMin, r.CPU.Min)
		wantMax = max(wantMax, r.CPU.Max)
	}
	if diff := mean - wantMean; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("window mean = %v, want %v", mean, wantMean)
	}
	if minv != wantMin || maxv != wantMax {
		t.Errorf("window envelope = %v..%v, want %v..%v", minv, maxv, wantMin, wantMax)
	}

	// Size against the estimate: DESIGN-DECISIONS works out 2 to 3 GB for
	// six months at full resolution, which is 77 to 115 MB a week. The
	// done-when tolerance is a factor of two either side.
	s.write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mb := float64(fi.Size()) / (1 << 20)
	t.Logf("database is %.0f MB after a week for %d hosts", mb, hosts)
	if mb < 77.0/2 || mb > 115.0*2 {
		t.Errorf("database is %.0f MB; the estimate allows %.0f to %.0f", mb, 77.0/2, 115.0*2)
	}
}

// synthReport is a deterministic, plausibly busy report: values vary by
// host and minute so roll-up arithmetic has something to aggregate, and the
// same function recomputes any window's expectation.
func synthReport(start time.Time, host, minute int) report.Report {
	v := func(scale int) float64 { return float64((minute*7+host*13+scale)%scale) + 1 }
	stat := func(scale int) report.Stat {
		m := v(scale)
		return report.Stat{Min: m * 0.5, Max: m * 1.5, Mean: m}
	}
	r := report.Report{
		Schema:  report.Schema,
		Version: "v1.0.0",
		Host:    report.Host{Hostname: fmt.Sprintf("host-%02d", host), Kernel: "6.8.0", CPUCount: 4, UptimeSeconds: int64(minute) * 60},
		Start:   start,
		End:     start.Add(50 * time.Second),
		Samples: 6,
		CPU:     stat(97),
		Load1:   stat(11), Load5: stat(7), Load15: stat(5),
		Memory: report.Gauge{Total: 8 << 30, Used: stat(89)},
		Swap:   report.Gauge{Total: 4 << 30, Used: stat(13)},
		Procs:  report.ProcCounts{Total: 200 + minute%17, Running: 3, Sleeping: 190, Zombie: minute % 3, Threads: 900, Kernel: 150},
		ContainerRuntime: "docker",
	}
	for i := range 3 {
		r.Networks = append(r.Networks, report.Network{
			Name: fmt.Sprintf("eth%d", i), Rx: stat(83 + i), Tx: stat(43 + i), Up: true,
		})
	}
	for i := range 2 {
		r.Disks = append(r.Disks, report.Disk{
			Name: fmt.Sprintf("nvme%dn1", i), Read: stat(71 + i), Write: stat(61 + i),
			ReadOps: stat(31 + i), WriteOps: stat(29 + i),
		})
	}
	for i := range 5 {
		r.Mounts = append(r.Mounts, report.Mount{
			Device: fmt.Sprintf("/dev/nvme0n1p%d", i+1), Path: fmt.Sprintf("/mnt/%d", i),
			FSType: "ext4", Total: 250 << 30, Used: uint64(minute) << 20, Percent: v(53),
		})
	}
	for i := range 5 {
		r.Containers = append(r.Containers, report.Container{
			Name: fmt.Sprintf("svc-%d", i), Image: "img:latest", State: "running",
			CPU: stat(37 + i), Mem: 100 << 20, MemLimit: 1 << 30,
		})
	}
	for i := range 8 {
		r.Top = append(r.Top, report.Process{
			PID: 1000 + i, Name: fmt.Sprintf("proc-%d", i), User: "app",
			CPU: stat(23 + i), RSS: 500 << 20, Swap: uint64(i),
		})
	}
	return r
}
