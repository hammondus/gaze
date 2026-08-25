package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/hammondus/mfa"
)

// Six digits across a three-step window is a small space, so the account,
// not the IP address, is what gets locked: per-IP limits are bypassed with a
// botnet. The one failures counter covers password and TOTP attempts alike —
// there is one account, and either factor failing repeatedly means the same
// thing.
const (
	maxFailures = 5
	lockout     = 15 * time.Minute
)

// adminSessionTTL bounds how long a signed-in browser stays signed in. A
// dashboard tab left open past it lands back at the login form.
const adminSessionTTL = 12 * time.Hour

// aad binds a sealed TOTP secret to the admin row it belongs in. mfa.Open
// refuses to decrypt under a different value, so a secret copied onto
// another row decrypts to nothing.
func adminAAD(id int64) []byte {
	return []byte("gaze_admin_totp:" + strconv.FormatInt(id, 10))
}

// AdminCount reports how many confirmed admins exist. Zero is what arms the
// web setup flow; an unconfirmed row from an abandoned setup does not count.
func (s *Store) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.write.QueryRowContext(ctx,
		`SELECT count(*) FROM admins WHERE totp_confirmed_at IS NOT NULL`).Scan(&n)
	return n, err
}

// BeginSetup creates the sole admin, unconfirmed, and returns the base32
// TOTP secret for enrolment. An abandoned earlier setup is replaced rather
// than collided with; a confirmed admin already existing is refused here as
// well as in the handler, so the store cannot be talked into a second one.
func (s *Store) BeginSetup(ctx context.Context, key []byte, username, password string) (string, error) {
	if username == "" {
		return "", fmt.Errorf("username is empty")
	}

	secret := mfa.NewSecret()
	raw, err := mfa.DecodeSecret(secret)
	if err != nil {
		return "", err
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var confirmed int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM admins WHERE totp_confirmed_at IS NOT NULL`).Scan(&confirmed); err != nil {
		return "", err
	}
	if confirmed > 0 {
		return "", fmt.Errorf("an admin already exists")
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM admins WHERE totp_confirmed_at IS NULL`); err != nil {
		return "", err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO admins (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, hashPassword(password), s.now().Unix())
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}

	box, err := mfa.Seal(key, raw, adminAAD(id))
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE admins SET totp_sealed = ? WHERE id = ?`, box, id); err != nil {
		return "", err
	}
	return secret, tx.Commit()
}

// PendingSetup returns the unconfirmed admin's username and base32 secret,
// which is what the QR page renders. ok is false when no setup is in
// progress; the secret is only reachable during the window it has to be.
func (s *Store) PendingSetup(ctx context.Context, key []byte) (username, secret string, ok bool, err error) {
	var id int64
	var box []byte
	err = s.write.QueryRowContext(ctx,
		`SELECT id, username, totp_sealed FROM admins WHERE totp_confirmed_at IS NULL`).
		Scan(&id, &username, &box)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", false, nil
	case err != nil:
		return "", "", false, err
	}
	raw, err := mfa.Open(key, box, adminAAD(id))
	if err != nil {
		return "", "", false, err
	}
	return username, mfa.EncodeSecret(raw), true, nil
}

// ConfirmSetup verifies the first code against the pending secret and, if
// it matches, confirms the admin. Requiring one valid code before the
// account is live is what stops a wrong clock or a mis-scanned QR code from
// locking the operator out at the very first login. The confirming code is
// spent like any other, so the same six digits cannot be replayed at the
// login prompt moments later.
func (s *Store) ConfirmSetup(ctx context.Context, key []byte, code string) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var id, lastStep int64
	var box []byte
	err = tx.QueryRowContext(ctx,
		`SELECT id, totp_sealed, totp_last_step FROM admins WHERE totp_confirmed_at IS NULL`).
		Scan(&id, &box, &lastStep)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil // no setup in progress
	case err != nil:
		return false, err
	}

	raw, err := mfa.Open(key, box, adminAAD(id))
	if err != nil {
		return false, err
	}
	acc, ok := mfa.VerifyTOTP(raw, code, s.now(), uint64(lastStep))
	if !ok {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE admins SET totp_last_step = ?, totp_confirmed_at = ?,
		        failures = 0, locked_until = NULL
		  WHERE id = ?`,
		int64(acc.Step), s.now().Unix(), id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CheckAdminPassword verifies the password and applies the lockout. An
// unknown username, a locked account, and a wrong password are
// indistinguishable to the caller by design, and all cost the same time:
// the hash is verified even when the result is already decided.
func (s *Store) CheckAdminPassword(ctx context.Context, username, password string) (int64, bool, error) {
	var (
		id          int64
		hash        string
		failures    int64
		lockedUntil sql.NullInt64
		confirmedAt sql.NullInt64
	)
	err := s.write.QueryRowContext(ctx,
		`SELECT id, password_hash, failures, locked_until, totp_confirmed_at
		   FROM admins WHERE username = ?`, username).
		Scan(&id, &hash, &failures, &lockedUntil, &confirmedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		verifyPassword(dummyHash(), password) // spend the same time
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}

	now := s.now()
	locked := lockedUntil.Valid && now.Unix() < lockedUntil.Int64
	ok := verifyPassword(hash, password)
	if locked || !confirmedAt.Valid {
		// An unconfirmed admin is half-created; setup, not login, is how it
		// finishes. Verified anyway, then discarded, so the reply time says
		// nothing.
		return 0, false, nil
	}

	if !ok {
		failures++
		var until any // nil writes NULL
		if failures >= maxFailures {
			until = now.Add(lockout).Unix()
		}
		_, err = s.write.ExecContext(ctx,
			`UPDATE admins SET failures = ?, locked_until = ? WHERE id = ?`,
			failures, until, id)
		return 0, false, err
	}

	if _, err := s.write.ExecContext(ctx,
		`UPDATE admins SET failures = 0, locked_until = NULL WHERE id = ?`, id); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// CheckAdminTOTP verifies code for the admin and spends it. A locked
// account and a wrong code both come back false; telling someone their
// lockout has started tells them when to come back.
func (s *Store) CheckAdminTOTP(ctx context.Context, key []byte, id int64, code string) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var (
		box                []byte
		lastStep, failures int64
		lockedUntil        sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT totp_sealed, totp_last_step, failures, locked_until
		   FROM admins WHERE id = ? AND totp_confirmed_at IS NOT NULL`, id).
		Scan(&box, &lastStep, &failures, &lockedUntil)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}

	now := s.now()
	if lockedUntil.Valid && now.Unix() < lockedUntil.Int64 {
		return false, nil
	}

	raw, err := mfa.Open(key, box, adminAAD(id))
	if err != nil {
		return false, err
	}

	acc, ok := mfa.VerifyTOTP(raw, code, now, uint64(lastStep))
	if !ok {
		failures++
		var until any
		if failures >= maxFailures {
			until = now.Add(lockout).Unix()
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE admins SET failures = ?, locked_until = ? WHERE id = ?`,
			failures, until, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}

	// Spending the step is not optional: without this write the code stays
	// valid for the rest of the skew window.
	if _, err := tx.ExecContext(ctx,
		`UPDATE admins SET totp_last_step = ?, failures = 0, locked_until = NULL
		  WHERE id = ?`, int64(acc.Step), id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AdminSession is a cookie-backed session. Authed distinguishes a session
// that has cleared both factors from one that has only cleared the password.
type AdminSession struct {
	ID      []byte // SHA-256 of the cookie token
	AdminID int64
	Authed  bool
	Expires time.Time
}

// NewAdminSession creates a session and returns the token for the cookie.
// It is always created unauthed: TOTP is mandatory, so no path skips the
// code prompt. Only the SHA-256 of the token is stored — same shape, and
// same reasoning, as agent tokens.
func (s *Store) NewAdminSession(ctx context.Context, adminID int64) (string, error) {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	id := sha256.Sum256([]byte(token))

	now := s.now()
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO admin_sessions (id, admin_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		id[:], adminID, now.Unix(), now.Add(adminSessionTTL).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// AdminSessionByToken resolves a cookie token. ok is false for an unknown
// or expired token; an expired row is deleted on the way past.
func (s *Store) AdminSessionByToken(ctx context.Context, token string) (AdminSession, bool, error) {
	id := sha256.Sum256([]byte(token))

	var (
		sess     AdminSession
		expires  int64
		authedAt sql.NullInt64
	)
	err := s.write.QueryRowContext(ctx,
		`SELECT id, admin_id, expires_at, authed_at FROM admin_sessions WHERE id = ?`,
		id[:]).Scan(&sess.ID, &sess.AdminID, &expires, &authedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AdminSession{}, false, nil
	case err != nil:
		return AdminSession{}, false, err
	}

	if s.now().Unix() >= expires {
		_ = s.DeleteAdminSession(ctx, sess.ID)
		return AdminSession{}, false, nil
	}
	sess.Authed = authedAt.Valid
	sess.Expires = time.Unix(expires, 0)
	return sess, true, nil
}

// PromoteAdminSession marks the second factor as cleared. Nothing else may
// set authed_at, so every full session ran through a verified code.
func (s *Store) PromoteAdminSession(ctx context.Context, id []byte) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE admin_sessions SET authed_at = ? WHERE id = ?`, s.now().Unix(), id)
	return err
}

func (s *Store) DeleteAdminSession(ctx context.Context, id []byte) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, id)
	return err
}

// ResetAdmin deletes an admin, sessions included through the cascade, which
// re-arms the web setup flow on the next server start. This is the recovery
// path for a lost authenticator or a forgotten password: the operator has
// shell access to the server by definition, so a CLI reset replaces the
// recovery-code machinery a hosted service would need. With one admin the
// username may be omitted; with several it must be named.
func (s *Store) ResetAdmin(ctx context.Context, username string) (string, error) {
	if username == "" {
		var n int
		if err := s.write.QueryRowContext(ctx, `SELECT count(*) FROM admins`).Scan(&n); err != nil {
			return "", err
		}
		switch n {
		case 0:
			return "", fmt.Errorf("no admin exists")
		case 1:
			if err := s.write.QueryRowContext(ctx, `SELECT username FROM admins`).Scan(&username); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("%d admins exist; name the one to reset", n)
		}
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM admins WHERE username = ?`, username)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("no admin named %q", username)
	}
	return username, nil
}
