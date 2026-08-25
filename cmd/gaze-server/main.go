// Command gaze-server ingests and stores reports from gaze-agents, and
// serves the web front end over them. The SSH TUI arrives in stage 6.
//
// Usage:
//
//	GAZE_KEY=<32 base64 bytes> gaze-server -db /data/gaze.db -addr :8080
//	gaze-server enroll <hostname> [-db /data/gaze.db]
//	gaze-server admin reset [username] [-db /data/gaze.db]
//
// enroll prints the new host's bearer token exactly once; the database
// keeps only its hash. admin reset deletes the admin account, which re-arms
// the first-run web setup on the next server start — the recovery path for
// a lost authenticator or a forgotten password.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/mfa"
)

var version = ""

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		enroll(os.Args[2:])
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "admin" && os.Args[2] == "reset" {
		adminReset(os.Args[3:])
		return
	}

	dbPath := flag.String("db", "gaze.db", "`path` to the SQLite database")
	addr := flag.String("addr", ":8080", "listen `address`")
	insecureCookies := flag.Bool("insecure-cookies", false,
		"drop the Secure attribute from cookies, for plain-HTTP local development only")
	sshAddr := flag.String("ssh-addr", "",
		"listen `address` for the SSH TUI; empty leaves SSH off")
	sshKeys := flag.String("ssh-authorized-keys", "",
		"`path` to the SSH allow-list, authorized_keys format (default: authorized_keys beside the database)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("gaze-server", buildVersion())
		return
	}

	key, err := loadKey()
	if err != nil {
		fatal("%v", err)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		fatal("%v", err)
	}
	defer s.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The sweep advances roll-ups and applies retention. Five minutes
	// matches the finest roll-up window; running it more often finds
	// nothing new.
	go func() {
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for {
			if err := s.Sweep(ctx); err != nil {
				log.Printf("sweep: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	web, err := newWebServer(s, key, !*insecureCookies)
	if err != nil {
		fatal("%v", err)
	}

	// The SSH TUI is its own listener, off by default and independent of
	// the web front end. Its host key lives beside the database, so a
	// restart never changes the server's SSH identity.
	if *sshAddr != "" {
		keysPath := *sshKeys
		if keysPath == "" {
			keysPath = filepath.Join(filepath.Dir(*dbPath), "authorized_keys")
		}
		hostKey := filepath.Join(filepath.Dir(*dbPath), "ssh_host_key")
		sshSrv, err := newSSHServer(query.New(s.Read()), *sshAddr, hostKey, keysPath)
		if err != nil {
			fatal("ssh: %v", err)
		}
		go func() {
			if err := sshSrv.serve(ctx); err != nil {
				log.Printf("ssh: %v", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/reports", newIngest(s))
	mux.Handle("/", web.handler())

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Argon2id at 64 MiB is deliberately slow, so the timeouts leave
		// room for it under a few concurrent logins.
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Printf("gaze-server %s: listening on %s, database %s", buildVersion(), *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("%v", err)
	}
}

// enroll mints a host token at the command line. The stage 5 web page wraps
// the same store call; this form exists first so ingest is testable before
// the front end, and stays for headless setups.
func enroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	dbPath := fs.String("db", "gaze.db", "`path` to the SQLite database")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gaze-server enroll <hostname> [-db path]")
		fs.PrintDefaults()
	}

	// Accept the hostname before or after the flags.
	var name string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		os.Exit(2)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		fatal("%v", err)
	}
	defer s.Close()

	token, err := s.Enroll(context.Background(), name)
	if err != nil {
		fatal("enroll %s: %v", name, err)
	}
	fmt.Printf(`host %q enrolled. Its token, shown this once:

  %s

On the host, as root:

  install -d -m 0750 -o gaze -g gaze /etc/gaze
  (umask 077; echo "%s" > /etc/gaze/token) && chown gaze:gaze /etc/gaze/token
`, name, token, token)
}

// loadKey reads GAZE_KEY, the AES-256 key that seals TOTP secrets. It is
// required: the alternative of generating one beside the database would
// store the key next to the only thing it protects. It comes from the
// environment at deploy time — see compose.yml.
func loadKey() ([]byte, error) {
	env := strings.TrimSpace(os.Getenv("GAZE_KEY"))
	if env == "" {
		return nil, fmt.Errorf("GAZE_KEY is not set. Generate one with:\n\n  head -c 32 /dev/urandom | base64\n\nand pass it in the environment — never store it beside the database")
	}
	key, err := base64.StdEncoding.DecodeString(env)
	if err != nil {
		return nil, fmt.Errorf("GAZE_KEY is not base64: %v", err)
	}
	if len(key) != mfa.KeySize {
		return nil, fmt.Errorf("GAZE_KEY must decode to %d bytes, got %d", mfa.KeySize, len(key))
	}
	return key, nil
}

// adminReset deletes the admin account at the command line. The next
// server start logs a fresh setup code and the web setup flow runs again.
func adminReset(args []string) {
	fs := flag.NewFlagSet("admin reset", flag.ExitOnError)
	dbPath := fs.String("db", "gaze.db", "`path` to the SQLite database")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gaze-server admin reset [username] [-db path]")
		fs.PrintDefaults()
	}

	// Accept the username before or after the flags, like enroll.
	var name string
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		fatal("%v", err)
	}
	defer s.Close()

	removed, err := s.ResetAdmin(context.Background(), name)
	if err != nil {
		fatal("admin reset: %v", err)
	}
	fmt.Printf("admin %q removed, sessions included. Restart the server; it logs a new setup code and /setup provisions the replacement.\n", removed)
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gaze-server: "+format+"\n", args...)
	os.Exit(1)
}
