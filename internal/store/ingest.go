package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/hammondus/gaze/internal/report"
)

// ErrUnknownToken is returned for a token with no row. The HTTP layer maps
// it to 401; anything else is a server fault, not the agent's.
var ErrUnknownToken = errors.New("unknown token")

// Authenticate resolves a bearer token to its host and stamps
// last_used_at. The lookup is the indexed hash comparison the token design
// calls for — nothing slower is needed against 256 random bits.
func (s *Store) Authenticate(ctx context.Context, token string) (int64, error) {
	hash := sha256.Sum256([]byte(token))
	var hostID int64
	err := s.read.QueryRowContext(ctx,
		`SELECT host_id FROM tokens WHERE hash = ?`, hash[:]).Scan(&hostID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUnknownToken
	}
	if err != nil {
		return 0, err
	}
	_, err = s.write.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE hash = ?`, s.now().Unix(), hash[:])
	return hostID, err
}

// InsertReports stores a posted batch for one host and returns how many
// reports were stored. The whole batch is one transaction.
//
// Sample times run on the agent's clock, so two clamps apply — see "Reports
// carry the agent's clock" in DESIGN-DECISIONS.md. A report from the future
// is recorded at receive time instead; one older than the raw retention
// window is skipped, because it would expire before the roll-up ever saw it.
//
// A newer wire schema than this binary knows is stored anyway: the decoder
// already dropped the fields it could not name, and the schema number lands
// on the row and the host so the mismatch is visible rather than fatal.
func (s *Store) InsertReports(ctx context.Context, hostID int64, batch []report.Report) (int, error) {
	now := s.now()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stored := 0
	for _, r := range batch {
		start, stop := r.Start.Unix(), r.End.Unix()
		if stop > now.Add(futureSkew).Unix() {
			start, stop = now.Unix(), now.Unix()
		}
		if stop < now.Add(-KeepRaw).Unix() {
			continue
		}
		if err := insertReport(ctx, tx, hostID, r, start, stop, now.Unix()); err != nil {
			return 0, err
		}
		stored++
	}
	if len(batch) > 0 {
		last := batch[len(batch)-1]
		if _, err := tx.ExecContext(ctx, `
			UPDATE hosts SET kernel = ?, cpus = ?, agent_version = ?,
			                 generation = ?, schema = ?, last_seen_at = ?
			WHERE id = ?`,
			last.Host.Kernel, last.Host.CPUCount, last.Version,
			last.Generation, last.Schema, now.Unix(), hostID); err != nil {
			return 0, err
		}
	}
	return stored, tx.Commit()
}

func insertReport(ctx context.Context, tx *sql.Tx, hostID int64, r report.Report, start, stop, receivedAt int64) error {
	// INSERT OR REPLACE keeps redelivery idempotent: an agent whose POST
	// succeeded but whose reply was lost sends the same report again.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO reports (
			host_id, tier, start, stop, received_at, samples, schema,
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
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hostID, TierRaw, start, stop, receivedAt, r.Samples, r.Schema,
		r.CPU.Min, r.CPU.Max, r.CPU.Mean,
		r.Load1.Min, r.Load1.Max, r.Load1.Mean,
		r.Load5.Min, r.Load5.Max, r.Load5.Mean,
		r.Load15.Min, r.Load15.Max, r.Load15.Mean,
		r.Memory.Total, r.Memory.Used.Min, r.Memory.Used.Max, r.Memory.Used.Mean,
		r.Swap.Total, r.Swap.Used.Min, r.Swap.Used.Max, r.Swap.Used.Mean,
		r.Host.UptimeSeconds,
		r.Procs.Total, r.Procs.Running, r.Procs.Sleeping, r.Procs.Stopped,
		r.Procs.Zombie, r.Procs.Threads, r.Procs.Kernel,
		r.ContainerRuntime, r.ContainersDisabled, strings.Join(r.Absent, ",")); err != nil {
		return err
	}

	for _, n := range r.Networks {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO net_reports (host_id, tier, start, samples, name,
				rx_min, rx_max, rx_mean, tx_min, tx_max, tx_mean, up)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			hostID, TierRaw, start, r.Samples, n.Name,
			n.Rx.Min, n.Rx.Max, n.Rx.Mean, n.Tx.Min, n.Tx.Max, n.Tx.Mean, n.Up); err != nil {
			return err
		}
	}
	for _, d := range r.Disks {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO disk_reports (host_id, tier, start, samples, name,
				read_min, read_max, read_mean, write_min, write_max, write_mean,
				rops_min, rops_max, rops_mean, wops_min, wops_max, wops_mean)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			hostID, TierRaw, start, r.Samples, d.Name,
			d.Read.Min, d.Read.Max, d.Read.Mean, d.Write.Min, d.Write.Max, d.Write.Mean,
			d.ReadOps.Min, d.ReadOps.Max, d.ReadOps.Mean, d.WriteOps.Min, d.WriteOps.Max, d.WriteOps.Mean); err != nil {
			return err
		}
	}
	for _, m := range r.Mounts {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO mount_reports (host_id, tier, start, samples, path,
				device, fstype, total, used, percent)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			hostID, TierRaw, start, r.Samples, m.Path,
			m.Device, m.FSType, m.Total, m.Used, m.Percent); err != nil {
			return err
		}
	}
	for _, c := range r.Containers {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO container_reports (host_id, tier, start, samples, name,
				image, state, cpu_min, cpu_max, cpu_mean, mem, mem_limit)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			hostID, TierRaw, start, r.Samples, c.Name,
			c.Image, c.State, c.CPU.Min, c.CPU.Max, c.CPU.Mean, c.Mem, c.MemLimit); err != nil {
			return err
		}
	}

	// NULL, not zero, for a swap figure the platform could not supply.
	swapAbsent := slices.Contains(r.Absent, "process.swap")
	for _, p := range r.Top {
		var swap any = p.Swap
		if swapAbsent {
			swap = nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO top_reports (host_id, start, samples, pid, name, user,
				cpu_min, cpu_max, cpu_mean, rss, swap, cmdline)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			hostID, start, r.Samples, p.PID, p.Name, p.User,
			p.CPU.Min, p.CPU.Max, p.CPU.Mean, p.RSS, swap, p.Cmdline); err != nil {
			return err
		}
	}
	return nil
}
