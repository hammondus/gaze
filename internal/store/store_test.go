package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
)

var base = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return base }
	return s
}

// testReport is a small report whose numbers are easy to predict.
func testReport(start time.Time, cpuMean float64) report.Report {
	return report.Report{
		Schema:  report.Schema,
		Version: "v9.9.9",
		Host:    report.Host{Hostname: "web-01", Kernel: "6.8.0", CPUCount: 4, UptimeSeconds: 1000},
		Start:   start,
		End:     start.Add(50 * time.Second),
		Samples: 6,
		CPU:     report.Stat{Min: cpuMean - 5, Max: cpuMean + 5, Mean: cpuMean},
		Load1:   report.Stat{Min: 1, Max: 2, Mean: 1.5},
		Memory:  report.Gauge{Total: 8 << 30, Used: report.Stat{Min: 1 << 30, Max: 2 << 30, Mean: 1.5 * (1 << 30)}},
		Swap:    report.Gauge{Total: 4 << 30, Used: report.Stat{Min: 0, Max: 0, Mean: 0}},
		Procs:   report.ProcCounts{Total: 100, Running: 2, Zombie: 1},
		Networks: []report.Network{
			{Name: "eth0", Rx: report.Stat{Min: 10, Max: 30, Mean: 20}, Tx: report.Stat{Min: 1, Max: 3, Mean: 2}, Up: true},
		},
		Disks: []report.Disk{
			{Name: "sda", Read: report.Stat{Min: 5, Max: 15, Mean: 10}},
		},
		Mounts: []report.Mount{
			{Device: "/dev/sda1", Path: "/", FSType: "ext4", Total: 100, Used: 50, Percent: 50},
		},
		Containers: []report.Container{
			{Name: "web", Image: "nginx", State: "running", CPU: report.Stat{Min: 1, Max: 3, Mean: 2}, Mem: 100, MemLimit: 200},
		},
		Top: []report.Process{
			{PID: 1, Name: "postgres", User: "postgres", CPU: report.Stat{Min: 10, Max: 20, Mean: 15}, RSS: 1 << 30, Swap: 5},
		},
		ContainerRuntime: "docker",
	}
}

func enrollHost(t *testing.T, s *Store, name string) (int64, string) {
	t.Helper()
	token, err := s.Enroll(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return id, token
}

// TestEnrollAndAuthenticate pins the token lifecycle: the token
// authenticates, garbage does not, and a second enrolment of the same host
// is a rotation, not an error.
func TestEnrollAndAuthenticate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, token := enrollHost(t, s, "web-01")
	if len(token) != 43 { // 32 bytes, base64url, unpadded
		t.Errorf("token is %d characters, want 43", len(token))
	}
	if _, err := s.Authenticate(ctx, "not-a-token"); err != ErrUnknownToken {
		t.Errorf("garbage token: err = %v, want ErrUnknownToken", err)
	}

	token2, err := s.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatalf("re-enrolling for rotation: %v", err)
	}
	id2, err := s.Authenticate(ctx, token2)
	if err != nil || id2 != id {
		t.Errorf("rotated token resolves to host %d, %v; want %d", id2, err, id)
	}

	// The clear token must not be in the database, only its hash.
	var n int
	if err := s.read.QueryRow(`SELECT count(*) FROM tokens WHERE hash = ?`, []byte(token)).Scan(&n); err != nil || n != 0 {
		t.Errorf("found %d rows storing the raw token (err %v)", n, err)
	}
	var lastUsed int64
	if err := s.read.QueryRow(`SELECT last_used_at FROM tokens ORDER BY id LIMIT 1`).Scan(&lastUsed); err != nil || lastUsed != base.Unix() {
		t.Errorf("last_used_at = %d, %v; want %d", lastUsed, err, base.Unix())
	}
}

// TestInsertReports covers storage, the hosts-row update, and idempotent
// redelivery.
func TestInsertReports(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	r := testReport(base.Add(-10*time.Minute), 50)
	for range 2 { // redelivery: same report twice is one row
		if n, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil || n != 1 {
			t.Fatalf("stored %d, %v", n, err)
		}
	}
	var rows int
	s.read.QueryRow(`SELECT count(*) FROM reports WHERE tier = 0`).Scan(&rows)
	if rows != 1 {
		t.Errorf("redelivered report stored %d rows, want 1", rows)
	}
	for table, want := range map[string]int{
		"net_reports": 1, "disk_reports": 1, "mount_reports": 1,
		"container_reports": 1, "top_reports": 1,
	} {
		var n int
		s.read.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n)
		if n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}

	var gen, schema int
	var agent string
	var seen int64
	s.read.QueryRow(`SELECT generation, schema, agent_version, last_seen_at FROM hosts WHERE id = ?`, id).
		Scan(&gen, &schema, &agent, &seen)
	if schema != report.Schema || agent != "v9.9.9" || seen != base.Unix() {
		t.Errorf("hosts row: schema %d agent %q seen %d", schema, agent, seen)
	}
}

// TestInsertClampsClocks pins the two clock rules: the future is clamped to
// receive time, and anything past raw retention is skipped.
func TestInsertClampsClocks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	future := testReport(base.Add(time.Hour), 50)
	ancient := testReport(base.Add(-8*24*time.Hour), 50)
	n, err := s.InsertReports(ctx, id, []report.Report{future, ancient})
	if err != nil || n != 1 {
		t.Fatalf("stored %d, %v; want 1 (the clamped one)", n, err)
	}
	var start, stop int64
	s.read.QueryRow(`SELECT start, stop FROM reports WHERE tier = 0`).Scan(&start, &stop)
	if start != base.Unix() || stop != base.Unix() {
		t.Errorf("future report stored at %d..%d, want clamped to %d", start, stop, base.Unix())
	}
}

// TestInsertNewerSchema pins the tolerance direction stage 4 added: a
// schema this binary does not know is stored, and the mismatch is recorded
// on the host.
func TestInsertNewerSchema(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	r := testReport(base.Add(-time.Minute), 50)
	r.Schema = report.Schema + 5
	if n, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil || n != 1 {
		t.Fatalf("stored %d, %v", n, err)
	}
	var schema int
	s.read.QueryRow(`SELECT schema FROM hosts WHERE id = ?`, id).Scan(&schema)
	if schema != report.Schema+5 {
		t.Errorf("hosts.schema = %d, want %d recorded", schema, report.Schema+5)
	}
}

// TestAbsentSwapStoresNull pins NULL-not-zero for a field the platform
// could not supply.
func TestAbsentSwapStoresNull(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	r := testReport(base.Add(-time.Minute), 50)
	r.Absent = []string{"process.swap"}
	if _, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil {
		t.Fatal(err)
	}
	var nulls int
	s.read.QueryRow(`SELECT count(*) FROM top_reports WHERE swap IS NULL`).Scan(&nulls)
	if nulls != 1 {
		t.Errorf("%d NULL swap rows, want 1: absent must not become zero", nulls)
	}
}

// TestRollup is the aggregation arithmetic: five raw reports become one
// five-minute row with min of mins, max of maxes, and the weighted mean —
// and the five-minute row becomes the hour's in the same sweep.
func TestRollup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	t0 := base.Add(-4 * time.Hour) // aligned to both windows
	means := []float64{10, 20, 30, 40, 50}
	for i, m := range means {
		r := testReport(t0.Add(time.Duration(i)*time.Minute), m)
		if _, err := s.InsertReports(ctx, id, []report.Report{r}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	var samples int
	var cpuMin, cpuMax, cpuMean float64
	err := s.read.QueryRow(`
		SELECT samples, cpu_min, cpu_max, cpu_mean FROM reports
		WHERE host_id = ? AND tier = 1 AND start = ?`, id, t0.Unix()).
		Scan(&samples, &cpuMin, &cpuMax, &cpuMean)
	if err != nil {
		t.Fatalf("no five-minute row at %v: %v", t0, err)
	}
	if samples != 30 || cpuMin != 5 || cpuMax != 55 || cpuMean != 30 {
		t.Errorf("tier 1: samples %d cpu %v/%v/%v, want 30, 5/55/30", samples, cpuMin, cpuMax, cpuMean)
	}

	err = s.read.QueryRow(`
		SELECT samples, cpu_min, cpu_max, cpu_mean FROM reports
		WHERE host_id = ? AND tier = 2 AND start = ?`, id, t0.Unix()).
		Scan(&samples, &cpuMin, &cpuMax, &cpuMean)
	if err != nil {
		t.Fatalf("no hourly row at %v: %v", t0, err)
	}
	if samples != 30 || cpuMean != 30 {
		t.Errorf("tier 2: samples %d mean %v, want 30 and 30", samples, cpuMean)
	}

	// A second sweep finds nothing new and changes nothing.
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.read.QueryRow(`SELECT count(*) FROM reports WHERE tier = 1`).Scan(&rows)
	if rows != 1 {
		t.Errorf("second sweep grew tier 1 to %d rows", rows)
	}

	// The series tables rolled with the host row.
	var rxMean float64
	if err := s.read.QueryRow(`
		SELECT rx_mean FROM net_reports WHERE host_id = ? AND tier = 1 AND start = ?`,
		id, t0.Unix()).Scan(&rxMean); err != nil || rxMean != 20 {
		t.Errorf("tier 1 eth0 rx mean = %v, %v; want 20", rxMean, err)
	}
}

// TestRollupWaitsOutTheDelay pins the two-hour hold: a window young enough
// that a backlog could still land in it is not rolled.
func TestRollupWaitsOutTheDelay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	recent := testReport(base.Add(-30*time.Minute), 50)
	if _, err := s.InsertReports(ctx, id, []report.Report{recent}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.read.QueryRow(`SELECT count(*) FROM reports WHERE tier > 0`).Scan(&rows)
	if rows != 0 {
		t.Errorf("a 30-minute-old report was rolled up %d rows early", rows)
	}
}

// TestRetention pins the deletes: each tier keeps its own horizon, and the
// busiest processes live only as long as the raw tier.
func TestRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := enrollHost(t, s, "web-01")

	old := testReport(base.Add(-6*24*time.Hour), 50) // inside raw retention
	if _, err := s.InsertReports(ctx, id, []report.Report{old}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(ctx); err != nil { // rolls it up while raw still exists
		t.Fatal(err)
	}

	// Two days later the raw row and its top processes have expired; the
	// roll-ups survive.
	s.now = func() time.Time { return base.Add(2 * 24 * time.Hour) }
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, q := range []struct {
		key, sql string
	}{
		{"raw", `SELECT count(*) FROM reports WHERE tier = 0`},
		{"rolled", `SELECT count(*) FROM reports WHERE tier > 0`},
		{"top", `SELECT count(*) FROM top_reports`},
	} {
		var n int
		s.read.QueryRow(q.sql).Scan(&n)
		counts[q.key] = n
	}
	if counts["raw"] != 0 || counts["top"] != 0 {
		t.Errorf("expired rows survive: raw %d, top %d", counts["raw"], counts["top"])
	}
	if counts["rolled"] == 0 {
		t.Error("the roll-ups expired with the raw tier; history is gone")
	}
}

// TestMigrateRefusesNewerDatabase pins the forward-only rule.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaze.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("opening a newer database: err = %v, want a refusal", err)
	}
}
