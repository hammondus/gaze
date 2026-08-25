package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Enroll creates the host if needed, mints it a bearer token, and returns
// the token. This is the only moment the token exists in the clear: the
// database keeps its SHA-256 and the caller prints it once.
//
// Enrolling an existing host adds a second token rather than failing, which
// is how a token is rotated: mint, deploy, then revoke the old one.
func (s *Store) Enroll(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("host name is empty")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	now := s.now().Unix()

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO hosts (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`,
		name, now); err != nil {
		return "", err
	}
	var hostID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM hosts WHERE name = ?`, name).Scan(&hostID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tokens (host_id, hash, created_at) VALUES (?, ?, ?)`,
		hostID, hash[:], now); err != nil {
		return "", err
	}
	return token, tx.Commit()
}
