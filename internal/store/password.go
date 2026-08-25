package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// The admin password is hashed with Argon2id. The reasoning that makes plain
// SHA-256 right for agent tokens does not transfer: a token carries 256
// random bits, a password carries whatever a person chose, and only a slow
// hash makes the difference up.
//
// These parameters are the RFC 9106 second recommended option: 64 MiB, one
// pass, four lanes. The PHC format below records the parameters with each
// hash, so old hashes keep verifying after a change.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// dummyHash is verified against when the username is unknown, so a login
// attempt costs the same whether or not the account exists. Lazy, because
// hashing costs 64 MiB and the enroll and reset subcommands never need it.
var dummyHash = sync.OnceValue(func() string {
	return hashPassword("password-that-is-never-correct")
})

// hashPassword returns a PHC-format string, the same shape the reference
// Argon2 tools emit:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
func hashPassword(plain string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	sum := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(sum))
}

// verifyPassword reports whether plain produced stored. Parameters come from
// stored rather than from the constants above, so hashes written under older
// settings still verify.
func verifyPassword(stored, plain string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	got := argon2.IDKey([]byte(plain), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
