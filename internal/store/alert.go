package store

import (
	"context"
	"time"
)

// Alert states, in escalation order. The state machine lives in
// internal/alert; the store only persists rows.
const (
	AlertOK      = 0
	AlertPending = 1
	AlertFiring  = 2
)

// AlertState is one rule's standing for one host (and instance, for rules
// that fan out per mount).
type AlertState struct {
	HostID   int64
	Rule     string
	Instance string
	State    int
	Since    time.Time // sample clock: when the current breach began
	Changed  time.Time // server clock: last transition
	Mailed   time.Time // server clock: last message; zero if never
}

// AlertStates returns every alert row for a host, keyed by rule and
// instance.
func (s *Store) AlertStates(ctx context.Context, hostID int64) (map[[2]string]AlertState, error) {
	rows, err := s.write.QueryContext(ctx, `
		SELECT rule, instance, state, since, changed_at, COALESCE(mailed_at, 0)
		FROM alert_state WHERE host_id = ?`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[[2]string]AlertState{}
	for rows.Next() {
		st := AlertState{HostID: hostID}
		var since, changed, mailed int64
		if err := rows.Scan(&st.Rule, &st.Instance, &st.State, &since, &changed, &mailed); err != nil {
			return nil, err
		}
		st.Since = time.Unix(since, 0)
		st.Changed = time.Unix(changed, 0)
		if mailed > 0 {
			st.Mailed = time.Unix(mailed, 0)
		}
		out[[2]string{st.Rule, st.Instance}] = st
	}
	return out, rows.Err()
}

// SetAlertState writes one row, replacing whatever was there.
func (s *Store) SetAlertState(ctx context.Context, st AlertState) error {
	var mailed any
	if !st.Mailed.IsZero() {
		mailed = st.Mailed.Unix()
	}
	_, err := s.write.ExecContext(ctx, `
		INSERT OR REPLACE INTO alert_state
			(host_id, rule, instance, state, since, changed_at, mailed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		st.HostID, st.Rule, st.Instance, st.State,
		st.Since.Unix(), st.Changed.Unix(), mailed)
	return err
}

// DeleteAlertState removes a row: the rule's subject is gone (an unmounted
// filesystem), so there is no state left to hold.
func (s *Store) DeleteAlertState(ctx context.Context, hostID int64, rule, instance string) error {
	_, err := s.write.ExecContext(ctx,
		`DELETE FROM alert_state WHERE host_id = ? AND rule = ? AND instance = ?`,
		hostID, rule, instance)
	return err
}

// Heartbeat is what the staleness sweep reads: a host and when the server
// last heard from it. LastSeen is receive time on purpose — a host with a
// broken clock must still be able to go stale.
type Heartbeat struct {
	HostID   int64
	Name     string
	LastSeen time.Time // zero if the host has never reported
}

// Heartbeats lists every host's last receive time.
func (s *Store) Heartbeats(ctx context.Context) ([]Heartbeat, error) {
	rows, err := s.write.QueryContext(ctx,
		`SELECT id, name, COALESCE(last_seen_at, 0) FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Heartbeat
	for rows.Next() {
		var h Heartbeat
		var seen int64
		if err := rows.Scan(&h.HostID, &h.Name, &seen); err != nil {
			return nil, err
		}
		if seen > 0 {
			h.LastSeen = time.Unix(seen, 0)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
