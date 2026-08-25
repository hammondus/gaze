// Package query reconstructs per-host views from stored reports. It is the
// seam between storage and presentation: the web front end (stage 5) and the
// SSH TUI (stage 6) both call it, and neither writes SQL of its own.
//
// Sample time (reports.start) orders and selects everything here; staleness
// is the one question asked of receive time, through Host.LastSeen.
package query

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

// Q answers read queries. It holds the store's read pool, so queries run
// concurrently with the single writer under WAL.
type Q struct {
	db *sql.DB
}

func New(db *sql.DB) *Q { return &Q{db: db} }

// Host is one row of the fleet list.
type Host struct {
	ID           int64
	Name         string
	Kernel       string
	CPUs         int
	AgentVersion string
	Generation   int
	Schema       int
	LastSeen     time.Time // zero if the host has never reported
}

// Hosts lists every enrolled host, never-reported ones included: a host
// that has never reported and one that stopped are both facts the fleet
// list exists to show.
func (q *Q) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT id, name, kernel, cpus, agent_version, generation, schema,
		       COALESCE(last_seen_at, 0)
		FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Host
	for rows.Next() {
		var h Host
		var seen int64
		if err := rows.Scan(&h.ID, &h.Name, &h.Kernel, &h.CPUs,
			&h.AgentVersion, &h.Generation, &h.Schema, &seen); err != nil {
			return nil, err
		}
		if seen > 0 {
			h.LastSeen = time.Unix(seen, 0)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Overview is one row of the fleet list: the host plus its latest raw
// report's headline figures. A host that has never reported keeps
// HasReport false and its figures zero — the renderer draws dashes, never
// zeros, for those.
type Overview struct {
	Host
	HasReport bool
	CPU       float64 // mean percent over the latest report
	MemUsed   float64 // mean bytes
	MemTotal  uint64
	Procs     int
	Zombies   int

	// Remote-management standing: the desired configuration generation
	// beside the echoed one in Host.Generation, the agent's refusal text,
	// and whether a self-update stands requested. The fleet list must show
	// a declined directive as plainly as an applied one.
	CfgGeneration int
	Declined      string
	UpdateAsked   bool
}

// Fleet returns every enrolled host with its latest raw report, in name
// order. This is the host-list query: one round trip for the whole page.
func (q *Q) Fleet(ctx context.Context) ([]Overview, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT h.id, h.name, h.kernel, h.cpus, h.agent_version, h.generation,
		       h.schema, COALESCE(h.last_seen_at, 0),
		       h.cfg_generation, h.declined,
		       h.update_requested_at IS NOT NULL,
		       r.cpu_mean, r.mem_mean, r.mem_total, r.procs, r.procs_zombie
		FROM hosts h
		LEFT JOIN reports r ON r.host_id = h.id AND r.tier = 0
		 AND r.start = (SELECT max(start) FROM reports
		                 WHERE host_id = h.id AND tier = 0)
		ORDER BY h.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Overview
	for rows.Next() {
		var o Overview
		var seen int64
		var cpu, mem sql.NullFloat64
		var memTotal, procs, zombies sql.NullInt64
		if err := rows.Scan(&o.ID, &o.Name, &o.Kernel, &o.CPUs,
			&o.AgentVersion, &o.Generation, &o.Schema, &seen,
			&o.CfgGeneration, &o.Declined, &o.UpdateAsked,
			&cpu, &mem, &memTotal, &procs, &zombies); err != nil {
			return nil, err
		}
		if seen > 0 {
			o.LastSeen = time.Unix(seen, 0)
		}
		if cpu.Valid {
			o.HasReport = true
			o.CPU = cpu.Float64
			o.MemUsed = mem.Float64
			o.MemTotal = uint64(memTotal.Int64)
			o.Procs = int(procs.Int64)
			o.Zombies = int(zombies.Int64)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Point is one aggregated observation of a host's scalars.
type Point struct {
	Start     time.Time
	Samples   int
	CPU       report.Stat
	Load1     report.Stat
	MemTotal  uint64
	Mem       report.Stat // used bytes
	SwapTotal uint64
	Swap      report.Stat
	Procs     int
	Zombies   int

	// Absent names the fields the host's platform could not supply for
	// this window, as metrics.Field values. A graph draws those as absent,
	// never as a zero line.
	Absent []string
}

// Scalars returns a host's aggregate series over [from, to), reading the
// finest tier whose retention still covers from. The tiers exist so this
// choice is automatic: callers state the range they want, not the
// resolution it survives at.
func (q *Q) Scalars(ctx context.Context, hostID int64, from, to time.Time) ([]Point, error) {
	tier := tierFor(from)
	rows, err := q.db.QueryContext(ctx, `
		SELECT start, samples, cpu_min, cpu_max, cpu_mean,
		       load1_min, load1_max, load1_mean,
		       mem_total, mem_min, mem_max, mem_mean,
		       swap_total, swap_min, swap_max, swap_mean,
		       procs, procs_zombie, absent
		FROM reports
		WHERE host_id = ? AND tier = ? AND start >= ? AND start < ?
		ORDER BY start`,
		hostID, tier, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var p Point
		var start int64
		var absent string
		if err := rows.Scan(&start, &p.Samples,
			&p.CPU.Min, &p.CPU.Max, &p.CPU.Mean,
			&p.Load1.Min, &p.Load1.Max, &p.Load1.Mean,
			&p.MemTotal, &p.Mem.Min, &p.Mem.Max, &p.Mem.Mean,
			&p.SwapTotal, &p.Swap.Min, &p.Swap.Max, &p.Swap.Mean,
			&p.Procs, &p.Zombies, &absent); err != nil {
			return nil, err
		}
		p.Start = time.Unix(start, 0)
		if absent != "" {
			p.Absent = strings.Split(absent, ",")
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NetSeries is one interface's rates over a range.
type NetSeries struct {
	Name   string
	Points []NetPoint
}

// NetPoint is one aggregated observation of an interface.
type NetPoint struct {
	Start  time.Time
	Rx, Tx report.Stat // bytes per second
}

// Nets returns a host's per-interface series over [from, to), name-ordered,
// at the same tier Scalars reads.
func (q *Q) Nets(ctx context.Context, hostID int64, from, to time.Time) ([]NetSeries, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT name, start, rx_min, rx_max, rx_mean, tx_min, tx_max, tx_mean
		FROM net_reports
		WHERE host_id = ? AND tier = ? AND start >= ? AND start < ?
		ORDER BY name, start`,
		hostID, tierFor(from), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NetSeries
	for rows.Next() {
		var name string
		var start int64
		var p NetPoint
		if err := rows.Scan(&name, &start,
			&p.Rx.Min, &p.Rx.Max, &p.Rx.Mean,
			&p.Tx.Min, &p.Tx.Max, &p.Tx.Mean); err != nil {
			return nil, err
		}
		p.Start = time.Unix(start, 0)
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, NetSeries{Name: name})
		}
		last := &out[len(out)-1]
		last.Points = append(last.Points, p)
	}
	return out, rows.Err()
}

// DiskSeries is one block device's rates over a range.
type DiskSeries struct {
	Name   string
	Points []DiskPoint
}

// DiskPoint is one aggregated observation of a device.
type DiskPoint struct {
	Start       time.Time
	Read, Write report.Stat // bytes per second
}

// Disks returns a host's per-device series over [from, to), name-ordered,
// at the same tier Scalars reads.
func (q *Q) Disks(ctx context.Context, hostID int64, from, to time.Time) ([]DiskSeries, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT name, start, read_min, read_max, read_mean,
		       write_min, write_max, write_mean
		FROM disk_reports
		WHERE host_id = ? AND tier = ? AND start >= ? AND start < ?
		ORDER BY name, start`,
		hostID, tierFor(from), from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DiskSeries
	for rows.Next() {
		var name string
		var start int64
		var p DiskPoint
		if err := rows.Scan(&name, &start,
			&p.Read.Min, &p.Read.Max, &p.Read.Mean,
			&p.Write.Min, &p.Write.Max, &p.Write.Mean); err != nil {
			return nil, err
		}
		p.Start = time.Unix(start, 0)
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, DiskSeries{Name: name})
		}
		last := &out[len(out)-1]
		last.Points = append(last.Points, p)
	}
	return out, rows.Err()
}

// tierFor picks the finest tier whose retention reaches back to from.
func tierFor(from time.Time) int {
	age := time.Since(from)
	switch {
	case age <= store.KeepRaw:
		return store.TierRaw
	case age <= store.KeepFiveMin:
		return store.TierFiveMin
	default:
		return store.TierHourly
	}
}

// Latest reconstructs the most recent stored report for a host, series and
// busiest processes included. Stage 6 renders this as a Snapshot-shaped
// view; fields a report never carried stay absent there, not zero.
func (q *Q) Latest(ctx context.Context, hostID int64) (*report.Report, error) {
	var r report.Report
	var start, stop, uptime int64
	var absent string
	err := q.db.QueryRowContext(ctx, `
		SELECT start, stop, samples, schema,
		       cpu_min, cpu_max, cpu_mean,
		       load1_min, load1_max, load1_mean,
		       load5_min, load5_max, load5_mean,
		       load15_min, load15_max, load15_mean,
		       mem_total, mem_min, mem_max, mem_mean,
		       swap_total, swap_min, swap_max, swap_mean,
		       uptime_s,
		       procs, procs_running, procs_sleeping, procs_stopped,
		       procs_zombie, procs_threads, procs_kernel,
		       container_runtime, containers_disabled, absent
		FROM reports WHERE host_id = ? AND tier = ?
		ORDER BY start DESC LIMIT 1`, hostID, store.TierRaw).Scan(
		&start, &stop, &r.Samples, &r.Schema,
		&r.CPU.Min, &r.CPU.Max, &r.CPU.Mean,
		&r.Load1.Min, &r.Load1.Max, &r.Load1.Mean,
		&r.Load5.Min, &r.Load5.Max, &r.Load5.Mean,
		&r.Load15.Min, &r.Load15.Max, &r.Load15.Mean,
		&r.Memory.Total, &r.Memory.Used.Min, &r.Memory.Used.Max, &r.Memory.Used.Mean,
		&r.Swap.Total, &r.Swap.Used.Min, &r.Swap.Used.Max, &r.Swap.Used.Mean,
		&uptime,
		&r.Procs.Total, &r.Procs.Running, &r.Procs.Sleeping, &r.Procs.Stopped,
		&r.Procs.Zombie, &r.Procs.Threads, &r.Procs.Kernel,
		&r.ContainerRuntime, &r.ContainersDisabled, &absent)
	if err != nil {
		return nil, err
	}
	r.Start, r.End = time.Unix(start, 0), time.Unix(stop, 0)
	r.Host.UptimeSeconds = uptime
	if absent != "" {
		r.Absent = strings.Split(absent, ",")
	}

	if err := q.latestSeries(ctx, hostID, start, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (q *Q) latestSeries(ctx context.Context, hostID, start int64, r *report.Report) error {
	rows, err := q.db.QueryContext(ctx, `
		SELECT name, rx_min, rx_max, rx_mean, tx_min, tx_max, tx_mean, up
		FROM net_reports WHERE host_id = ? AND tier = 0 AND start = ? ORDER BY name`,
		hostID, start)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n report.Network
		if err := rows.Scan(&n.Name, &n.Rx.Min, &n.Rx.Max, &n.Rx.Mean,
			&n.Tx.Min, &n.Tx.Max, &n.Tx.Mean, &n.Up); err != nil {
			rows.Close()
			return err
		}
		r.Networks = append(r.Networks, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.db.QueryContext(ctx, `
		SELECT name, read_min, read_max, read_mean, write_min, write_max, write_mean,
		       rops_min, rops_max, rops_mean, wops_min, wops_max, wops_mean
		FROM disk_reports WHERE host_id = ? AND tier = 0 AND start = ? ORDER BY name`,
		hostID, start)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d report.Disk
		if err := rows.Scan(&d.Name, &d.Read.Min, &d.Read.Max, &d.Read.Mean,
			&d.Write.Min, &d.Write.Max, &d.Write.Mean,
			&d.ReadOps.Min, &d.ReadOps.Max, &d.ReadOps.Mean,
			&d.WriteOps.Min, &d.WriteOps.Max, &d.WriteOps.Mean); err != nil {
			rows.Close()
			return err
		}
		r.Disks = append(r.Disks, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.db.QueryContext(ctx, `
		SELECT path, device, fstype, total, used, percent
		FROM mount_reports WHERE host_id = ? AND tier = 0 AND start = ? ORDER BY path`,
		hostID, start)
	if err != nil {
		return err
	}
	for rows.Next() {
		var m report.Mount
		if err := rows.Scan(&m.Path, &m.Device, &m.FSType, &m.Total, &m.Used, &m.Percent); err != nil {
			rows.Close()
			return err
		}
		r.Mounts = append(r.Mounts, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.db.QueryContext(ctx, `
		SELECT name, image, state, cpu_min, cpu_max, cpu_mean, mem, mem_limit
		FROM container_reports WHERE host_id = ? AND tier = 0 AND start = ? ORDER BY name`,
		hostID, start)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c report.Container
		if err := rows.Scan(&c.Name, &c.Image, &c.State,
			&c.CPU.Min, &c.CPU.Max, &c.CPU.Mean, &c.Mem, &c.MemLimit); err != nil {
			rows.Close()
			return err
		}
		r.Containers = append(r.Containers, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.db.QueryContext(ctx, `
		SELECT pid, name, user, cpu_min, cpu_max, cpu_mean, rss,
		       COALESCE(swap, 0), cmdline
		FROM top_reports WHERE host_id = ? AND start = ?
		ORDER BY cpu_mean DESC, pid`,
		hostID, start)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p report.Process
		if err := rows.Scan(&p.PID, &p.Name, &p.User,
			&p.CPU.Min, &p.CPU.Max, &p.CPU.Mean, &p.RSS, &p.Swap, &p.Cmdline); err != nil {
			return err
		}
		r.Top = append(r.Top, p)
	}
	return rows.Err()
}
