// Command gaze-server ingests and stores reports from gaze-agents. The web
// front end arrives in stage 5 and the SSH TUI in stage 6; today this is
// the ingest endpoint, the roll-up sweep, and the enroll command.
//
// Usage:
//
//	gaze-server -db /data/gaze.db -addr :8080
//	gaze-server enroll <hostname> [-db /data/gaze.db]
//
// enroll prints the new host's bearer token exactly once; the database
// keeps only its hash.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/hammondus/gaze/internal/store"
)

var version = ""

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		enroll(os.Args[2:])
		return
	}

	dbPath := flag.String("db", "gaze.db", "`path` to the SQLite database")
	addr := flag.String("addr", ":8080", "listen `address`")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("gaze-server", buildVersion())
		return
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

	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/reports", newIngest(s))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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
