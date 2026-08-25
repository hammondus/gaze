// Package store owns the server's database: the schema, its migrations, and
// every write — enrolment, ingest, roll-up, and retention. Reads for
// presentation live in internal/query; the HTTP layer calls both and writes
// no SQL of its own.
//
// Storage is SQLite through modernc.org/sqlite in WAL mode, with one writer
// — see "SQLite, and what six months costs" in DESIGN-DECISIONS.md.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Tiers of stored resolution. Raw reports roll up into five-minute rows,
// and five-minute rows into hourly ones.
const (
	TierRaw     = 0
	TierFiveMin = 1
	TierHourly  = 2
)

// Retention and roll-up geometry, from the table in DESIGN-DECISIONS.
const (
	KeepRaw     = 7 * 24 * time.Hour
	KeepFiveMin = 90 * 24 * time.Hour
	KeepHourly  = 2 * 365 * 24 * time.Hour

	// rollDelay is how far behind the wall clock the roll-up stays: twice
	// the agent's buffer horizon, so a backlog flushed after an outage is
	// never rolled up short.
	rollDelay = 2 * time.Hour

	// futureSkew is how far ahead of the server clock a report's sample
	// time may run before it is clamped to receive time.
	futureSkew = 5 * time.Minute
)

// Store is an open database. The write pool is a single connection, which is
// the "one writer" in the design; the read pool serves queries concurrently
// under WAL.
type Store struct {
	write *sql.DB
	read  *sql.DB

	// now is the server clock, replaceable so tests and the synthetic-load
	// harness can steer time.
	now func() time.Time
}

// Open opens or creates the database at path and applies any pending
// migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		return nil, err
	}

	s := &Store{write: write, read: read, now: time.Now}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	rerr := s.read.Close()
	werr := s.write.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// Read returns the read pool, which internal/query builds on.
func (s *Store) Read() *sql.DB { return s.read }

// migrate applies the forward-only migrations past the version recorded in
// PRAGMA user_version. There is no downgrade path on purpose: restoring an
// older binary against a newer database must fail loudly, not half-work.
func (s *Store) migrate() error {
	ctx := context.Background()
	var version int
	if err := s.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database is at schema %d, this binary knows %d: refusing to run backwards", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// migrations are applied in order, each in its own transaction, and are
// append-only: edit history here and every deployed database silently
// diverges from a fresh one.
var migrations = []string{`
CREATE TABLE hosts (
	id            INTEGER PRIMARY KEY,
	name          TEXT NOT NULL UNIQUE,
	created_at    INTEGER NOT NULL,             -- unix seconds, server clock
	kernel        TEXT NOT NULL DEFAULT '',
	cpus          INTEGER NOT NULL DEFAULT 0,
	agent_version TEXT NOT NULL DEFAULT '',
	generation    INTEGER NOT NULL DEFAULT 0,   -- last generation the agent echoed
	schema        INTEGER NOT NULL DEFAULT 0,   -- last wire schema the agent posted
	last_seen_at  INTEGER                       -- server clock; staleness reads this
);

-- Stage 5 fills this; the table exists now because retrofitting a users
-- table after rows reference a single admin is the expensive direction.
CREATE TABLE admins (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	totp_sealed   BLOB,
	failures      INTEGER NOT NULL DEFAULT 0,
	locked_until  INTEGER,
	created_at    INTEGER NOT NULL
);

-- hash is unsalted SHA-256, looked up by index: a salt defends a guessable
-- secret, which 256 random bits are not, and a salted hash cannot be found
-- by index. See "Each host's token is its own" in DESIGN-DECISIONS.md.
CREATE TABLE tokens (
	id           INTEGER PRIMARY KEY,
	host_id      INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	hash         BLOB NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER
);

-- One row per report per tier. start and stop are the agent's sample clock;
-- received_at is the server's. Charts and roll-ups read start, staleness
-- reads hosts.last_seen_at. Counts (procs_*) carry the last observation at
-- tier 0 and the window peak in roll-ups.
CREATE TABLE reports (
	host_id     INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	tier        INTEGER NOT NULL,
	start       INTEGER NOT NULL,
	stop        INTEGER NOT NULL,
	received_at INTEGER NOT NULL,
	samples     INTEGER NOT NULL,
	schema      INTEGER NOT NULL,
	cpu_min REAL NOT NULL, cpu_max REAL NOT NULL, cpu_mean REAL NOT NULL,
	load1_min REAL NOT NULL, load1_max REAL NOT NULL, load1_mean REAL NOT NULL,
	load5_min REAL NOT NULL, load5_max REAL NOT NULL, load5_mean REAL NOT NULL,
	load15_min REAL NOT NULL, load15_max REAL NOT NULL, load15_mean REAL NOT NULL,
	mem_total INTEGER NOT NULL,
	mem_min REAL NOT NULL, mem_max REAL NOT NULL, mem_mean REAL NOT NULL,
	swap_total INTEGER NOT NULL,
	swap_min REAL NOT NULL, swap_max REAL NOT NULL, swap_mean REAL NOT NULL,
	uptime_s INTEGER NOT NULL,
	procs INTEGER NOT NULL, procs_running INTEGER NOT NULL,
	procs_sleeping INTEGER NOT NULL, procs_stopped INTEGER NOT NULL,
	procs_zombie INTEGER NOT NULL, procs_threads INTEGER NOT NULL,
	procs_kernel INTEGER NOT NULL,
	container_runtime TEXT NOT NULL DEFAULT '',
	containers_disabled INTEGER NOT NULL DEFAULT 0,
	absent TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (host_id, tier, start)
) WITHOUT ROWID;
CREATE INDEX reports_tier_start ON reports (tier, start);

CREATE TABLE net_reports (
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	tier INTEGER NOT NULL, start INTEGER NOT NULL, samples INTEGER NOT NULL,
	name TEXT NOT NULL,
	rx_min REAL NOT NULL, rx_max REAL NOT NULL, rx_mean REAL NOT NULL,
	tx_min REAL NOT NULL, tx_max REAL NOT NULL, tx_mean REAL NOT NULL,
	up INTEGER NOT NULL,
	PRIMARY KEY (host_id, tier, start, name)
) WITHOUT ROWID;
CREATE INDEX net_reports_tier_start ON net_reports (tier, start);

CREATE TABLE disk_reports (
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	tier INTEGER NOT NULL, start INTEGER NOT NULL, samples INTEGER NOT NULL,
	name TEXT NOT NULL,
	read_min REAL NOT NULL, read_max REAL NOT NULL, read_mean REAL NOT NULL,
	write_min REAL NOT NULL, write_max REAL NOT NULL, write_mean REAL NOT NULL,
	rops_min REAL NOT NULL, rops_max REAL NOT NULL, rops_mean REAL NOT NULL,
	wops_min REAL NOT NULL, wops_max REAL NOT NULL, wops_mean REAL NOT NULL,
	PRIMARY KEY (host_id, tier, start, name)
) WITHOUT ROWID;
CREATE INDEX disk_reports_tier_start ON disk_reports (tier, start);

-- Mounts carry last observations at tier 0 and window peaks in roll-ups:
-- peak usage is the number a capacity question wants.
CREATE TABLE mount_reports (
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	tier INTEGER NOT NULL, start INTEGER NOT NULL, samples INTEGER NOT NULL,
	path TEXT NOT NULL,
	device TEXT NOT NULL, fstype TEXT NOT NULL,
	total INTEGER NOT NULL, used INTEGER NOT NULL, percent REAL NOT NULL,
	PRIMARY KEY (host_id, tier, start, path)
) WITHOUT ROWID;
CREATE INDEX mount_reports_tier_start ON mount_reports (tier, start);

CREATE TABLE container_reports (
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	tier INTEGER NOT NULL, start INTEGER NOT NULL, samples INTEGER NOT NULL,
	name TEXT NOT NULL,
	image TEXT NOT NULL, state TEXT NOT NULL,
	cpu_min REAL NOT NULL, cpu_max REAL NOT NULL, cpu_mean REAL NOT NULL,
	mem INTEGER NOT NULL, mem_limit INTEGER NOT NULL,
	PRIMARY KEY (host_id, tier, start, name)
) WITHOUT ROWID;
CREATE INDEX container_reports_tier_start ON container_reports (tier, start);

-- The busiest processes are stored at raw resolution only: after the raw
-- tier expires, the host-level envelopes remain and per-process detail is
-- the thing deliberately let go. swap is NULL when the platform could not
-- supply it — NULL and zero are different facts.
CREATE TABLE top_reports (
	host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	start INTEGER NOT NULL, samples INTEGER NOT NULL,
	pid INTEGER NOT NULL, name TEXT NOT NULL, user TEXT NOT NULL,
	cpu_min REAL NOT NULL, cpu_max REAL NOT NULL, cpu_mean REAL NOT NULL,
	rss INTEGER NOT NULL, swap INTEGER, cmdline TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (host_id, start, pid)
) WITHOUT ROWID;
CREATE INDEX top_reports_start ON top_reports (start);

-- Roll-up progress per destination tier: windows starting before "until"
-- are done. One global watermark, not per host — the sweep rolls every
-- host's complete windows together.
CREATE TABLE rollup_state (
	tier  INTEGER PRIMARY KEY,
	until INTEGER NOT NULL
);
`, `
-- Stage 5: what the admins table was missing to actually sign someone in.
-- totp_last_step records the highest TOTP counter step already accepted, so
-- a code cannot be replayed inside the skew window; totp_confirmed_at is
-- NULL until the first valid code proves the authenticator was enrolled,
-- and an unconfirmed admin can neither sign in nor end the setup flow.
ALTER TABLE admins ADD COLUMN totp_last_step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE admins ADD COLUMN totp_confirmed_at INTEGER;

-- id is the SHA-256 of the cookie token, same shape as agent tokens: a
-- database dump holds no usable session credential, and the lookup stays
-- one indexed read. authed_at NULL means the password cleared but the TOTP
-- code has not: the session exists and reaches nothing but the code prompt.
CREATE TABLE admin_sessions (
	id         BLOB PRIMARY KEY,
	admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	authed_at  INTEGER
) WITHOUT ROWID;
`, `
-- Stage 7: alert state, one row per rule per host per instance (a mount
-- path, or '' for host-level rules). The rules themselves are code, in
-- internal/alert; persisting only the state means a restart neither
-- re-fires every open alert nor forgets one. since is the sample clock
-- (when the breach began); changed_at and mailed_at are the server clock,
-- and mailed_at is what re-notify suppression reads.
CREATE TABLE alert_state (
	host_id    INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	rule       TEXT NOT NULL,
	instance   TEXT NOT NULL DEFAULT '',
	state      INTEGER NOT NULL,
	since      INTEGER NOT NULL,
	changed_at INTEGER NOT NULL,
	mailed_at  INTEGER,
	PRIMARY KEY (host_id, rule, instance)
) WITHOUT ROWID;
`, `
-- Stage 8: the desired remote configuration per host, beside what the
-- agent has echoed. cfg_generation counts every change; hosts.generation
-- (migration 1) is the agent's echo, and the two being equal is what
-- "applied, not just sent" means. cfg_containers is NULL to leave the
-- agent's own setting alone — NULL and "off" are different instructions.
-- declined is the agent's own sentence for why it refused a directive.
ALTER TABLE hosts ADD COLUMN cfg_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hosts ADD COLUMN cfg_sample_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hosts ADD COLUMN cfg_report_s INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hosts ADD COLUMN cfg_containers INTEGER;
ALTER TABLE hosts ADD COLUMN update_requested_at INTEGER;
ALTER TABLE hosts ADD COLUMN declined TEXT NOT NULL DEFAULT '';
`}
