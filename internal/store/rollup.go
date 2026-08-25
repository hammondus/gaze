package store

import (
	"context"
	"fmt"
	"time"
)

// tierSpec describes one roll-up destination: which tier it reads, how wide
// its windows are, and how long its rows live.
type tierSpec struct {
	tier   int
	source int
	window int64 // seconds
	keep   time.Duration
}

var tiers = []tierSpec{
	{tier: TierFiveMin, source: TierRaw, window: 300, keep: KeepFiveMin},
	{tier: TierHourly, source: TierFiveMin, window: 3600, keep: KeepHourly},
}

// Sweep advances the roll-ups and applies retention. Call it on a timer; it
// is idempotent and does nothing when no window has become ready.
//
// Only windows ending before now minus two hours are rolled — twice the
// agent's buffer horizon — so a backlog flushed after an outage is never
// rolled up short. The hourly tier additionally waits for the five-minute
// tier it reads from, so it never aggregates a half-built window.
func (s *Store) Sweep(ctx context.Context) error {
	now := s.now()

	limit := now.Add(-rollDelay).Unix()
	for _, t := range tiers {
		if t.source != TierRaw {
			// Read only what the previous tier has finished.
			var srcUntil int64
			err := s.write.QueryRowContext(ctx,
				`SELECT COALESCE((SELECT until FROM rollup_state WHERE tier = ?), 0)`,
				t.source).Scan(&srcUntil)
			if err != nil {
				return err
			}
			limit = min(limit, srcUntil)
		}
		if err := s.rollTier(ctx, t, (limit/t.window)*t.window); err != nil {
			return fmt.Errorf("roll tier %d: %w", t.tier, err)
		}
	}

	return s.retain(ctx, now)
}

// rollTier aggregates the source tier's rows into t's windows for every
// window in [watermark, until), then advances the watermark. Aggregation is
// min of mins, max of maxes, and a sample-weighted mean, so the roll-up of
// a spike keeps the spike — see "SQLite, and what six months costs".
func (s *Store) rollTier(ctx context.Context, t tierSpec, until int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var from int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT until FROM rollup_state WHERE tier = ?1),
			(SELECT COALESCE(min(start), ?2) FROM reports WHERE tier = ?3)
		)`, t.tier, until, t.source).Scan(&from)
	if err != nil {
		return err
	}
	from = (from / t.window) * t.window
	if from >= until {
		return nil
	}

	args := []any{t.tier, t.window, t.source, from, until}
	weighted := func(col string) string {
		return fmt.Sprintf("sum(%s * samples) / CAST(sum(samples) AS REAL)", col)
	}

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
		)
		SELECT host_id, ?1, (start / ?2) * ?2, max(stop), max(received_at),
			sum(samples), min(schema),
			min(cpu_min), max(cpu_max), `+weighted("cpu_mean")+`,
			min(load1_min), max(load1_max), `+weighted("load1_mean")+`,
			min(load5_min), max(load5_max), `+weighted("load5_mean")+`,
			min(load15_min), max(load15_max), `+weighted("load15_mean")+`,
			max(mem_total), min(mem_min), max(mem_max), `+weighted("mem_mean")+`,
			max(swap_total), min(swap_min), max(swap_max), `+weighted("swap_mean")+`,
			max(uptime_s),
			max(procs), max(procs_running), max(procs_sleeping), max(procs_stopped),
			max(procs_zombie), max(procs_threads), max(procs_kernel),
			max(container_runtime), max(containers_disabled), max(absent)
		FROM reports
		WHERE tier = ?3 AND start >= ?4 AND start < ?5
		GROUP BY host_id, start / ?2`, args...); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO net_reports (host_id, tier, start, samples, name,
			rx_min, rx_max, rx_mean, tx_min, tx_max, tx_mean, up)
		SELECT host_id, ?1, (start / ?2) * ?2, sum(samples), name,
			min(rx_min), max(rx_max), `+weighted("rx_mean")+`,
			min(tx_min), max(tx_max), `+weighted("tx_mean")+`, max(up)
		FROM net_reports
		WHERE tier = ?3 AND start >= ?4 AND start < ?5
		GROUP BY host_id, start / ?2, name`, args...); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO disk_reports (host_id, tier, start, samples, name,
			read_min, read_max, read_mean, write_min, write_max, write_mean,
			rops_min, rops_max, rops_mean, wops_min, wops_max, wops_mean)
		SELECT host_id, ?1, (start / ?2) * ?2, sum(samples), name,
			min(read_min), max(read_max), `+weighted("read_mean")+`,
			min(write_min), max(write_max), `+weighted("write_mean")+`,
			min(rops_min), max(rops_max), `+weighted("rops_mean")+`,
			min(wops_min), max(wops_max), `+weighted("wops_mean")+`
		FROM disk_reports
		WHERE tier = ?3 AND start >= ?4 AND start < ?5
		GROUP BY host_id, start / ?2, name`, args...); err != nil {
		return err
	}

	// Mounts and containers keep window peaks: peak usage is the number a
	// capacity question asks for.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO mount_reports (host_id, tier, start, samples, path,
			device, fstype, total, used, percent)
		SELECT host_id, ?1, (start / ?2) * ?2, sum(samples), path,
			max(device), max(fstype), max(total), max(used), max(percent)
		FROM mount_reports
		WHERE tier = ?3 AND start >= ?4 AND start < ?5
		GROUP BY host_id, start / ?2, path`, args...); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO container_reports (host_id, tier, start, samples, name,
			image, state, cpu_min, cpu_max, cpu_mean, mem, mem_limit)
		SELECT host_id, ?1, (start / ?2) * ?2, sum(samples), name,
			max(image), max(state),
			min(cpu_min), max(cpu_max), `+weighted("cpu_mean")+`,
			max(mem), max(mem_limit)
		FROM container_reports
		WHERE tier = ?3 AND start >= ?4 AND start < ?5
		GROUP BY host_id, start / ?2, name`, args...); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rollup_state (tier, until) VALUES (?, ?)
		ON CONFLICT(tier) DO UPDATE SET until = excluded.until`,
		t.tier, until); err != nil {
		return err
	}
	return tx.Commit()
}

// retain deletes rows past each tier's retention. Top processes live as
// long as the raw tier: after that, the host-level envelopes are the
// history and per-process detail is deliberately let go.
func (s *Store) retain(ctx context.Context, now time.Time) error {
	cutoffs := []struct {
		tier int
		keep time.Duration
	}{
		{TierRaw, KeepRaw},
		{TierFiveMin, KeepFiveMin},
		{TierHourly, KeepHourly},
	}
	for _, c := range cutoffs {
		cutoff := now.Add(-c.keep).Unix()
		for _, table := range []string{"reports", "net_reports", "disk_reports", "mount_reports", "container_reports"} {
			if _, err := s.write.ExecContext(ctx,
				`DELETE FROM `+table+` WHERE tier = ? AND start < ?`, c.tier, cutoff); err != nil {
				return err
			}
		}
	}
	_, err := s.write.ExecContext(ctx,
		`DELETE FROM top_reports WHERE start < ?`, now.Add(-KeepRaw).Unix())
	return err
}
