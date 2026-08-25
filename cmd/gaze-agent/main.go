// Command gaze-agent samples the machine it runs on and posts reductions to
// a gaze-server. It collects on a short interval and reports on a long one,
// so a spike between reports still arrives — see "Sample often, report
// rarely" in DESIGN-DECISIONS.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/hammondus/gaze/internal/metrics"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = ""

func main() {
	server := flag.String("server", "", "server base `URL`; https, or http on loopback only")
	tokenFile := flag.String("token-file", "", "`path` to the bearer token file, mode 0600")
	sample := flag.Duration("sample", 10*time.Second, "collection `interval`")
	reportEvery := flag.Duration("report", time.Minute, "reporting `interval`")
	containers := flag.Bool("containers", true, "collect container statistics from the Docker or Podman socket")
	cmdlines := flag.Bool("cmdlines", false, "include process command lines in reports; they can carry secrets, so this is a local choice no server can switch on")
	allowRemoteConfig := flag.Bool("allow-remote-config", false, "let the server change the intervals and collection settings")
	procPath := flag.String("procfs", "/proc", "`path` to the proc filesystem")
	sysPath := flag.String("sysfs", "/sys", "`path` to the sys filesystem")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("gaze-agent", buildVersion())
		return
	}
	if *server == "" || *tokenFile == "" {
		fatal("both -server and -token-file are required")
	}
	base, err := checkServerURL(*server)
	if err != nil {
		fatal("%v", err)
	}
	if _, err := readToken(*tokenFile); err != nil {
		fatal("%v", err)
	}
	if *sample < time.Second {
		fatal("-sample must be at least 1s")
	}
	if *reportEvery < *sample {
		fatal("-report must be at least the -sample interval")
	}

	// With the paths left at their defaults, collection goes through
	// metrics.NewSource — the platform boundary. Explicit paths replay a
	// captured tree and must exist.
	newCollector := func(disableContainers bool) *metrics.Collector {
		return metrics.New(metrics.Options{DisableContainers: disableContainers})
	}
	if flagSet("procfs") || flagSet("sysfs") {
		for _, p := range []string{*procPath, *sysPath} {
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				fatal("cannot read directory %s", p)
			}
		}
		newCollector = func(disableContainers bool) *metrics.Collector {
			return metrics.NewWithSource(metrics.Source{
				Proc: os.DirFS(*procPath),
				Sys:  os.DirFS(*sysPath),
			}, metrics.Options{DisableContainers: disableContainers})
		}
	}

	a := &agent{
		client:            newClient(base, *tokenFile, buildVersion()),
		version:           buildVersion(),
		hostID:            hostID(),
		cmdlines:          *cmdlines,
		allowRemoteConfig: *allowRemoteConfig,
		newCollector:      newCollector,
		cfg: config{
			sample:        *sample,
			report:        *reportEvery,
			containersOff: !*containers,
		},
		wake: make(chan struct{}, 1),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("gaze-agent %s: sampling every %s, reporting to %s every %s (offset %s)",
		buildVersion(), *sample, base.Redacted(), *reportEvery, a.offset(*reportEvery).Round(time.Second))
	a.run(ctx)
}

// hostID is a stable identifier for deriving this host's reporting offset.
// Uniqueness barely matters — two hosts sharing an offset merely post
// together — so the fallbacks are fine; stability across restarts is the
// property the offset needs.
func hostID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "gaze"
}

func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// buildVersion prefers the linker-supplied version, then the module version
// the toolchain recorded, and admits to neither rather than inventing one.
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
	fmt.Fprintf(os.Stderr, "gaze-agent: "+format+"\n", args...)
	os.Exit(1)
}
