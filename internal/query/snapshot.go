package query

import (
	"context"
	"slices"
	"time"

	"github.com/hammondus/gaze/internal/metrics"
)

// LatestSnapshot reconstructs a metrics.Snapshot-shaped view of a host from
// its newest stored report, for the SSH front end to hand to the same
// renderer the local TUI uses.
//
// The reconstruction is honest about being a reduced view: a report is
// deliberately lossy (see "The wire format is not the snapshot" in
// DESIGN-DECISIONS.md), so fields it never carried — per-core CPU, sensors,
// the full process table, per-process CPU time — come back empty or listed
// in Absent, never as fabricated zeros. Aggregated figures use the report's
// mean: the envelope exists for graphs; a gauge shows one number.
//
// A host that has never reported returns sql.ErrNoRows, the same way Latest
// does; the caller says "no reports yet" rather than drawing a screen of
// zeros that reads as an idle machine.
func (q *Q) LatestSnapshot(ctx context.Context, hostID int64) (metrics.Snapshot, error) {
	var name, kernel string
	var cpus int
	if err := q.db.QueryRowContext(ctx,
		`SELECT name, kernel, cpus FROM hosts WHERE id = ?`, hostID).
		Scan(&name, &kernel, &cpus); err != nil {
		return metrics.Snapshot{}, err
	}

	r, err := q.Latest(ctx, hostID)
	if err != nil {
		return metrics.Snapshot{}, err
	}

	s := metrics.Snapshot{
		Taken:    r.End,
		Interval: r.End.Sub(r.Start),
		Host: metrics.Host{
			Hostname: name,
			Kernel:   kernel,
			CPUCount: cpus,
			Uptime:   time.Duration(r.Host.UptimeSeconds) * time.Second,
		},
		CPU:  metrics.CPU{Name: "cpu", Busy: r.CPU.Mean},
		Load: metrics.Load{One: r.Load1.Mean, Five: r.Load5.Mean, Fifteen: r.Load15.Mean},
		Memory: metrics.Memory{
			Total:     r.Memory.Total,
			Used:      uint64(r.Memory.Used.Mean),
			Available: r.Memory.Total - uint64(r.Memory.Used.Mean),
			Percent:   pct(r.Memory.Used.Mean, r.Memory.Total),
		},
		Swap: metrics.Swap{
			Total:   r.Swap.Total,
			Used:    uint64(r.Swap.Used.Mean),
			Free:    r.Swap.Total - uint64(r.Swap.Used.Mean),
			Percent: pct(r.Swap.Used.Mean, r.Swap.Total),
		},
		ProcCounts: metrics.ProcCounts{
			Total:    r.Procs.Total,
			Running:  r.Procs.Running,
			Sleeping: r.Procs.Sleeping,
			Stopped:  r.Procs.Stopped,
			Zombie:   r.Procs.Zombie,
			Threads:  r.Procs.Threads,
			Kernel:   r.Procs.Kernel,
		},
		ContainerRuntime:   r.ContainerRuntime,
		ContainersDisabled: r.ContainersDisabled,

		// Sensors never cross the wire, so they are absent here by
		// construction, on top of whatever the host's own platform could
		// not supply.
		Absent: withField(toFields(r.Absent), metrics.FieldSensors),
	}

	for _, n := range r.Networks {
		s.Networks = append(s.Networks, metrics.Network{
			Name: n.Name, RxRate: n.Rx.Mean, TxRate: n.Tx.Mean, Up: n.Up,
		})
	}
	for _, d := range r.Disks {
		s.Disks = append(s.Disks, metrics.Disk{
			Name: d.Name, ReadRate: d.Read.Mean, WriteRate: d.Write.Mean,
			IOPSRead: d.ReadOps.Mean, IOPSWrite: d.WriteOps.Mean,
		})
	}
	for _, m := range r.Mounts {
		s.Mounts = append(s.Mounts, metrics.Mount{
			Device: m.Device, Path: m.Path, FSType: m.FSType,
			Total: m.Total, Used: m.Used, Free: m.Total - m.Used,
			Percent: m.Percent,
		})
	}
	for _, p := range r.Top {
		s.Processes = append(s.Processes, metrics.Process{
			PID: p.PID, Name: p.Name, User: p.User, Cmdline: p.Cmdline,
			CPU: p.CPU.Mean, RSS: p.RSS, Swap: p.Swap,
			MemPct: pct(float64(p.RSS), r.Memory.Total),
		})
	}
	for _, c := range r.Containers {
		s.Containers = append(s.Containers, metrics.Container{
			Name: c.Name, Image: c.Image, State: c.State,
			CPU: c.CPU.Mean, MemUsed: c.Mem, MemLimit: c.MemLimit,
			MemPct: pct(float64(c.Mem), c.MemLimit),
		})
	}
	return s, nil
}

func pct(used float64, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return used / float64(total) * 100
}

func toFields(names []string) []metrics.Field {
	out := make([]metrics.Field, 0, len(names)+1)
	for _, n := range names {
		out = append(out, metrics.Field(n))
	}
	return out
}

// withField appends f unless the report already declared it.
func withField(fields []metrics.Field, f metrics.Field) []metrics.Field {
	if slices.Contains(fields, f) {
		return fields
	}
	return append(fields, f)
}
