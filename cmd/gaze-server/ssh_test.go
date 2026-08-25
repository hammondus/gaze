package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"golang.org/x/crypto/ssh"
)

// newClientKey generates one client keypair and returns the signer and its
// authorized_keys line.
func newClientKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer, ssh.MarshalAuthorizedKey(sshPub)
}

// sshFixture is a running SSH front end over a seeded store.
type sshFixture struct {
	addr   string
	server *sshServer
	client ssh.Signer // a key on the allow-list
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(filepath.Join(dir, "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// One reporting host and one silent one, so the list has something to
	// say.
	ctx := context.Background()
	if _, err := s.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}
	token, err := s.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Report{
		Schema: report.Schema,
		Host:   report.Host{Hostname: "web-01", Kernel: "6.8.0", CPUCount: 4, UptimeSeconds: 7200},
		Start:  time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		CPU:    report.Stat{Min: 10, Max: 50, Mean: 25},
		Memory: report.Gauge{Total: 8 << 30, Used: report.Stat{Mean: 3 << 30}},
		Procs:  report.ProcCounts{Total: 187},
		Top:    []report.Process{{PID: 812, Name: "postgres", User: "pg", CPU: report.Stat{Mean: 9}, RSS: 1 << 30}},
	}
	if _, err := s.InsertReports(ctx, hostID, []report.Report{r}); err != nil {
		t.Fatal(err)
	}

	client, authorized := newClientKey(t)
	keysPath := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(keysPath, authorized, 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := newSSHServer(query.New(s.Read()), "127.0.0.1:0",
		filepath.Join(dir, "ssh_host_key"), keysPath)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.serveListener(serveCtx, ln)

	return &sshFixture{addr: ln.Addr().String(), server: srv, client: client}
}

func (f *sshFixture) dial(t *testing.T, key ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            "gaze",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(f.server.signer.PublicKey()),
		Timeout:         5 * time.Second,
	})
}

// TestHostKeyPersists is the restart rule: the server's SSH identity must
// not change, or every operator learns to accept host-key warnings.
func TestHostKeyPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh_host_key")

	first, err := loadHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode = %v, %v", fi.Mode(), err)
	}
	second, err := loadHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("host key changed across restarts")
	}
}

func TestAuthorizedKeysFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")

	_, a := newClientKey(t)
	_, b := newClientKey(t)
	if err := os.WriteFile(path, append(a, b...), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("parsed %d keys, want 2", len(keys))
	}

	// A malformed line refuses the whole file rather than quietly
	// shrinking the allow-list to its readable prefix.
	if err := os.WriteFile(path, append(a, []byte("not a key\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthorizedKeys(path); err == nil {
		t.Fatal("malformed allow-list parsed without error")
	}
}

// TestUnlistedKeyRefused is the done-when's second half: a key not on the
// allow-list fails in the handshake, before a shell — or a TUI — exists.
func TestUnlistedKeyRefused(t *testing.T) {
	f := newSSHFixture(t)
	intruder, _ := newClientKey(t)
	if client, err := f.dial(t, intruder); err == nil {
		client.Close()
		t.Fatal("an unlisted key authenticated")
	}
}

// TestSSHSession is the done-when's first half: an authorized key drops
// into the host list, and the list shows the fleet.
func TestSSHSession(t *testing.T) {
	f := newSSHFixture(t)
	client, err := f.dial(t, f.client)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// Collect output until the fleet list has drawn both hosts.
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	// waitAfter scans only output produced after mark, so a wait cannot be
	// satisfied by a stale frame that scrolled past earlier.
	waitAfter := func(mark int, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			s := out.String()
			mu.Unlock()
			if strings.Contains(s[min(mark, len(s)):], want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no %q in session output after byte %d:\n%s", want, mark, out.String())
	}
	waitFor := func(want string) { t.Helper(); waitAfter(0, want) }
	pos := func() int {
		mu.Lock()
		defer mu.Unlock()
		return out.Len()
	}

	waitFor("hosts")
	waitFor("web-01")
	waitFor("silent-01")
	waitFor("never reported")

	// A resize reaches the program as a window-change request.
	if err := sess.WindowChange(50, 150); err != nil {
		t.Fatal(err)
	}

	// The list is name-ordered, so the cursor starts on silent-01; one
	// step down is web-01, and enter opens it in the ordinary dashboard.
	fmt.Fprint(stdin, "j\r")
	waitFor("postgres")

	// q backs out to the list. The second q is sent only once the list has
	// re-rendered: two q bytes read in one chunk parse as a single "qq"
	// rune message, which matches no binding — the same as typing them
	// faster than a read anywhere in gaze.
	mark := pos()
	fmt.Fprint(stdin, "q")
	waitAfter(mark, "enter view")
	fmt.Fprint(stdin, "q")

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		tail := out.String()
		mu.Unlock()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		t.Fatalf("session did not end after q; output tail:\n%q", tail)
	}
}

// TestOneSessionPerConnection: a second session channel on the same
// connection is refused, per the one-TUI-per-connection rule.
func TestOneSessionPerConnection(t *testing.T) {
	f := newSSHFixture(t)
	client, err := f.dial(t, f.client)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	first, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if second, err := client.NewSession(); err == nil {
		second.Close()
		t.Fatal("a second session on one connection was accepted")
	}
}
