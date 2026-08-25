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
	UpdateAsked time.Time

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
	)
	err := s.write.QueryRowContext(ctx, `
		SELECT cfg_generation, cfg_sample_s, cfg_report_s, cfg_containers,
		       update_requested_at, generation, agent_version, declined
		FROM hosts WHERE id = ?`, hostID).
		Scan(&c.Generation, &c.SampleS, &c.ReportS, &containers,
			&asked, &c.Echoed, &c.AgentVersion, &c.Declined)
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
// stagger clock, which is what an operator pressing the button expects.
func (s *Store) RequestUpdate(ctx context.Context, hostID int64) error {
	res, err := s.write.ExecContext(ctx,
		`UPDATE hosts SET update_requested_at = ? WHERE id = ?`,
		s.now().Unix(), hostID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("no host %d", hostID)
	}
	return nil
}

// RequestUpdateAll marks every host. The stagger in the directive builder
// is what keeps the fleet from all fetching at once.
func (s *Store) RequestUpdateAll(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE hosts SET update_requested_at = ?`, s.now().Unix())
	return err
}

// ClearUpdateRequest ends a host's update request — the agent reached the
// version, so there is nothing left to ask for.
func (s *Store) ClearUpdateRequest(ctx context.Context, hostID int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE hosts SET update_requested_at = NULL WHERE id = ?`, hostID)
	return err
}
