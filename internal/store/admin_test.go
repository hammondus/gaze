package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hammondus/mfa"
)

var testKey = make([]byte, mfa.KeySize) // all-zero is a fine AES key for tests

// The mfa package verifies codes but does not generate them, which is
// correct for a server. These tests play the authenticator app, so HOTP is
// reimplemented from RFC 4226 section 5.3 — twelve lines, test-only.
func hotp(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// codeAt returns the TOTP code for the step `ahead` steps past at. The
// store's clock is fixed in these tests, so stepping forward is how a flow
// presents a second, unspent code without sleeping: VerifyTOTP accepts one
// step either side of now.
func codeAt(t *testing.T, secret string, at time.Time, ahead uint64) string {
	t.Helper()
	raw, err := mfa.DecodeSecret(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return hotp(raw, uint64(at.Unix())/30+ahead)
}

// setupAdmin drives the whole setup flow and returns the confirmed admin's
// id and secret.
func setupAdmin(t *testing.T, s *Store) (int64, string) {
	t.Helper()
	ctx := context.Background()
	secret, err := s.BeginSetup(ctx, testKey, "craig", "correct horse battery")
	if err != nil {
		t.Fatalf("BeginSetup: %v", err)
	}
	ok, err := s.ConfirmSetup(ctx, testKey, codeAt(t, secret, base, 0))
	if err != nil || !ok {
		t.Fatalf("ConfirmSetup: ok=%v err=%v", ok, err)
	}
	id, ok, err := s.CheckAdminPassword(ctx, "craig", "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("CheckAdminPassword after setup: ok=%v err=%v", ok, err)
	}
	return id, secret
}

func TestSetupFlow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if n, err := s.AdminCount(ctx); err != nil || n != 0 {
		t.Fatalf("AdminCount on fresh db = %d, %v", n, err)
	}

	// An abandoned setup is replaced, not collided with.
	if _, err := s.BeginSetup(ctx, testKey, "wrong-name", "abandoned"); err != nil {
		t.Fatalf("first BeginSetup: %v", err)
	}
	secret, err := s.BeginSetup(ctx, testKey, "craig", "correct horse battery")
	if err != nil {
		t.Fatalf("second BeginSetup: %v", err)
	}

	// The pending secret is readable for the QR page, and matches.
	name, pending, ok, err := s.PendingSetup(ctx, testKey)
	if err != nil || !ok || name != "craig" || pending != secret {
		t.Fatalf("PendingSetup = %q %q %v %v", name, pending, ok, err)
	}

	// An unconfirmed admin neither counts nor signs in.
	if n, _ := s.AdminCount(ctx); n != 0 {
		t.Fatalf("unconfirmed admin counted: %d", n)
	}
	if _, ok, _ := s.CheckAdminPassword(ctx, "craig", "correct horse battery"); ok {
		t.Fatal("unconfirmed admin signed in")
	}

	// A wrong code does not confirm; the right one does.
	if ok, err := s.ConfirmSetup(ctx, testKey, "000000"); ok || err != nil {
		t.Fatalf("wrong code confirmed: %v %v", ok, err)
	}
	if ok, err := s.ConfirmSetup(ctx, testKey, codeAt(t, secret, base, 0)); !ok || err != nil {
		t.Fatalf("ConfirmSetup: %v %v", ok, err)
	}
	if n, _ := s.AdminCount(ctx); n != 1 {
		t.Fatalf("AdminCount after confirm = %d", n)
	}

	// Once a confirmed admin exists, setup is over.
	if _, err := s.BeginSetup(ctx, testKey, "mallory", "hi"); err == nil {
		t.Fatal("BeginSetup allowed a second admin")
	}
	if _, _, ok, _ := s.PendingSetup(ctx, testKey); ok {
		t.Fatal("PendingSetup still pending after confirm")
	}
}

func TestPasswordLockout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	setupAdmin(t, s)

	// Unknown user and wrong password are the same false.
	if _, ok, _ := s.CheckAdminPassword(ctx, "nobody", "x"); ok {
		t.Fatal("unknown user accepted")
	}

	for range maxFailures {
		if _, ok, _ := s.CheckAdminPassword(ctx, "craig", "wrong"); ok {
			t.Fatal("wrong password accepted")
		}
	}
	// Locked: even the right password is refused.
	if _, ok, _ := s.CheckAdminPassword(ctx, "craig", "correct horse battery"); ok {
		t.Fatal("locked account accepted the right password")
	}

	// The lockout expires and the counter was not advanced by the locked-out
	// attempt.
	s.now = func() time.Time { return base.Add(lockout + time.Minute) }
	if _, ok, err := s.CheckAdminPassword(ctx, "craig", "correct horse battery"); !ok || err != nil {
		t.Fatalf("after lockout expiry: ok=%v err=%v", ok, err)
	}
}

func TestTOTPSpendAndLockout(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, secret := setupAdmin(t, s)

	// The confirming code spent step 0; the next step is fresh.
	c := codeAt(t, secret, base, 1)
	if ok, err := s.CheckAdminTOTP(ctx, testKey, id, c); !ok || err != nil {
		t.Fatalf("valid code: ok=%v err=%v", ok, err)
	}
	// Replay of the same code is refused.
	if ok, _ := s.CheckAdminTOTP(ctx, testKey, id, c); ok {
		t.Fatal("replayed code accepted")
	}

	for range maxFailures {
		if ok, _ := s.CheckAdminTOTP(ctx, testKey, id, "000000"); ok {
			t.Fatal("wrong code accepted")
		}
	}
	// Locked; move the clock forward and the account recovers. The step
	// window has moved with the clock, so a fresh code exists.
	later := base.Add(lockout + time.Minute)
	s.now = func() time.Time { return later }
	if ok, err := s.CheckAdminTOTP(ctx, testKey, id, codeAt(t, secret, later, 0)); !ok || err != nil {
		t.Fatalf("after lockout expiry: ok=%v err=%v", ok, err)
	}
}

func TestAdminSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _ := setupAdmin(t, s)

	token, err := s.NewAdminSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	sess, ok, err := s.AdminSessionByToken(ctx, token)
	if err != nil || !ok {
		t.Fatalf("AdminSessionByToken: ok=%v err=%v", ok, err)
	}
	if sess.Authed {
		t.Fatal("new session is already authed; TOTP is mandatory")
	}
	if sess.AdminID != id {
		t.Fatalf("session admin = %d, want %d", sess.AdminID, id)
	}

	if err := s.PromoteAdminSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if sess, _, _ = s.AdminSessionByToken(ctx, token); !sess.Authed {
		t.Fatal("promoted session not authed")
	}

	// A garbage token resolves to nothing.
	if _, ok, _ := s.AdminSessionByToken(ctx, "not-a-token"); ok {
		t.Fatal("garbage token resolved")
	}

	// Expiry: past the TTL the session is gone, and its row deleted.
	s.now = func() time.Time { return base.Add(adminSessionTTL + time.Minute) }
	if _, ok, _ := s.AdminSessionByToken(ctx, token); ok {
		t.Fatal("expired session resolved")
	}
	var n int
	if err := s.write.QueryRowContext(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expired session row survived: %d", n)
	}

	if err := s.DeleteAdminSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
}

func TestResetAdmin(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.ResetAdmin(ctx, ""); err == nil {
		t.Fatal("reset with no admin succeeded")
	}

	id, _ := setupAdmin(t, s)
	token, err := s.NewAdminSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ResetAdmin(ctx, "nobody"); err == nil {
		t.Fatal("reset of a missing username succeeded")
	}

	// Unnamed reset with exactly one admin deletes it, and the cascade takes
	// its sessions: setup is re-armed and the old cookie is dead.
	name, err := s.ResetAdmin(ctx, "")
	if err != nil || name != "craig" {
		t.Fatalf("ResetAdmin = %q, %v", name, err)
	}
	if n, _ := s.AdminCount(ctx); n != 0 {
		t.Fatalf("AdminCount after reset = %d", n)
	}
	if _, ok, _ := s.AdminSessionByToken(ctx, token); ok {
		t.Fatal("session survived the admin's deletion")
	}
}

// TestMigrationTwoAppliesOverStageFour builds a database that stops at the
// stage-4 schema, then reopens it through Open, which must carry it forward.
// The ALTER TABLE path is the risk migration 2 carries; a fresh database
// only ever exercises CREATE.
func TestMigrationTwoAppliesOverStageFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaze.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatalf("apply stage-4 schema: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open over stage-4 db: %v", err)
	}
	defer s.Close()
	s.now = func() time.Time { return base }

	// The migrated columns and table are usable end to end.
	setupAdmin(t, s)
}
