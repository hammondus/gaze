package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// HostConfig is one host's remote-management standing: what the operator
// asked for, and what the agent has said back.
type HostConfig struct {
	// Generation counts configuration changes; zero means no remote
	// configuration has ever been set for this host.
	Generation int

	// SampleS and ReportS are the desired intervals in seconds; zero
	// leaves the agent's own value alone. Containers nil likewise.
	SampleS    int
	ReportS    int
	Containers *bool

	// UpdateAsked is when a self-update was requested; zero means none is.
	// UpdateSent is when the server first sent the trigger for that
	// request; zero means not yet. UpdateError is the agent's text for why
	// its last attempt failed.
	UpdateAsked time.Time
	UpdateSent  time.Time
	UpdateError string

	// Echoed is the generation the agent last reported as applied, and
	// AgentVersion its build. Echoed == Generation is what "applied, not
	// just sent" means.
	Echoed       int
	AgentVersion string

	// Declined is the agent's own sentence for why it refused the last
	// directive it would not apply; empty means nothing stands refused.
	Declined string
}

// HostConfig reads one host's remote-management row.
func (s *Store) HostConfig(ctx context.Context, hostID int64) (HostConfig, error) {
	var (
		c          HostConfig
		containers sql.NullInt64
		asked      sql.NullInt64
		sent       sql.NullInt64
	)
	err := s.write.QueryRowContext(ctx, `
		SELECT cfg_generation, cfg_sample_s, cfg_report_s, cfg_containers,
		       update_requested_at, update_sent_at, update_error,
		       generation, agent_version, declined
		FROM hosts WHERE id = ?`, hostID).
		Scan(&c.Generation, &c.SampleS, &c.ReportS, &containers,
			&asked, &sent, &c.UpdateError,
			&c.Echoed, &c.AgentVersion, &c.Declined)
	if err != nil {
		return HostConfig{}, err
	}
	if containers.Valid {
		v := containers.Int64 != 0
		c.Containers = &v
	}
	if asked.Valid {
		c.UpdateAsked = time.Unix(asked.Int64, 0)
	}
	if sent.Valid {
		c.UpdateSent = time.Unix(sent.Int64, 0)
	}
	return c, nil
}

// SetHostConfig records the desired configuration and bumps the
// generation, which is what makes the change visible as sent — and, once
// the agent echoes it, as applied.
func (s *Store) SetHostConfig(ctx context.Context, hostID int64, sampleS, reportS int, containers *bool) (int, error) {
	var ctr any // nil writes NULL: leave the agent's own setting alone
	if containers != nil {
		ctr = 0
		if *containers {
			ctr = 1
		}
	}
	var gen int
	err := s.write.QueryRowContext(ctx, `
		UPDATE hosts SET cfg_generation = cfg_generation + 1,
		                 cfg_sample_s = ?, cfg_report_s = ?, cfg_containers = ?
		WHERE id = ?
		RETURNING cfg_generation`,
		sampleS, reportS, ctr, hostID).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("set config for host %d: %w", hostID, err)
	}
	return gen, nil
}

// RequestUpdate marks a host for a self-update. Asking again restarts the
// stagger clock and the progress, which is what an operator pressing the
// button expects.
func (s *Store) RequestUpdate(ctx context.Context, hostID int64) error {
	res, err := s.write.ExecContext(ctx,
		`UPDATE hosts SET update_requested_at = ?, update_sent_at = NULL WHERE id = ?`,
		s.now().Unix(), hostID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("no host %d", hostID)
	}
	return nil
}

// RequestUpdateAll marks every host that has reported and is not already
// on latest, and returns how many it marked. The stagger in the directive
// builder is what keeps the fleet from all fetching at once.
//
// latest is the newest release, or empty when the server could not learn
// it; empty skips no one, because then nobody is known to be current. A
// host that has never reported is skipped: it has no running agent to
// tell, and whichever version it starts with is the one it was installed
// with.
func (s *Store) RequestUpdateAll(ctx context.Context, latest string) (int, error) {
	res, err := s.write.ExecContext(ctx, `
		UPDATE hosts SET update_requested_at = ?, update_sent_at = NULL
		WHERE last_seen_at IS NOT NULL AND (? = '' OR agent_version != ?)`,
		s.now().Unix(), latest, latest)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// MarkUpdateSent records the first time the server put the update trigger
// on a reply for the standing request. Later re-sends keep the first time:
// "sent 4m ago" is the question the page answers.
func (s *Store) MarkUpdateSent(ctx context.Context, hostID int64) error {
	_, err := s.write.ExecContext(ctx, `
		UPDATE hosts SET update_sent_at = COALESCE(update_sent_at, ?)
		WHERE id = ? AND update_requested_at IS NOT NULL`,
		s.now().Unix(), hostID)
	return err
}

// ClearUpdateRequest ends a host's update request — the agent reached the
// version, so there is nothing left to ask for.
func (s *Store) ClearUpdateRequest(ctx context.Context, hostID int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE hosts SET update_requested_at = NULL, update_sent_at = NULL WHERE id = ?`, hostID)
	return err
}
