// Command gaze is a single-binary system monitor for Linux terminals.
//
// It reads /proc and /sys directly and depends on nothing outside the standard
// library to do so. See DESIGN-DECISIONS.md for why.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/hammondus/gaze/internal/metrics"
	"github.com/hammondus/gaze/internal/ui"
	"github.com/hammondus/gaze/internal/update"
)

// version is set at build time with -ldflags "-X main.version=…". Without it,
// the value falls back to whatever the module build recorded.
var version = ""

func main() {
	interval := flag.Duration("i", time.Second, "refresh `interval`")
	procPath := flag.String("procfs", "/proc", "`path` to the proc filesystem")
	sysPath := flag.String("sysfs", "/sys", "`path` to the sys filesystem")
	showVersion := flag.Bool("version", false, "print the version and exit")
	doUpdate := flag.Bool("update", false, "replace this executable with the latest release")
	checkUpdate := flag.Bool("check-update", false, "report whether a newer release exists, then exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("gaze", buildVersion())
		return
	}
	if *doUpdate || *checkUpdate {
		runUpdate(*doUpdate)
		return
	}
	if *interval < 100*time.Millisecond {
		fatal("refresh interval must be at least 100ms")
	}

	// Fail here rather than on a blank screen. Everything this program shows
	// comes from these two directories.
	if err := checkDir(*procPath); err != nil {
		fatal("%v", err)
	}
	if err := checkDir(*sysPath); err != nil {
		fatal("%v", err)
	}

	col := metrics.NewWithSource(metrics.Source{
		Proc: os.DirFS(*procPath),
		Sys:  os.DirFS(*sysPath),
	})

	p := tea.NewProgram(
		ui.New(col, *interval),
		tea.WithAltScreen(),       // leave the scrollback intact on exit
		tea.WithMouseCellMotion(), // wheel scrolling in the process list
	)
	if _, err := p.Run(); err != nil {
		fatal("%v", err)
	}
}

// Exit codes for --check-update. A script has to be able to tell "there is an
// update" apart from "I could not find out", or a network failure reads as an
// available update and the script does the wrong thing.
const (
	exitUpToDate    = 0
	exitUpdateFound = 1
	exitCannotCheck = 2
)

// runUpdate handles --update and --check-update.
func runUpdate(apply bool) {
	up, err := update.New(buildVersion())
	if err != nil {
		fatalCode(exitCannotCheck, "%v", err)
	}
	if apply {
		if err := up.Apply(os.Stdout); err != nil {
			fatalCode(exitCannotCheck, "%v", err)
		}
		return
	}

	latest, differs, err := up.Available()
	if err != nil {
		fatalCode(exitCannotCheck, "%v", err)
	}
	if !differs {
		fmt.Printf("gaze %s is the published version\n", latest)
		os.Exit(exitUpToDate)
	}
	fmt.Printf("gaze %s is available; this is %s\nrun: gaze --update\n", latest, buildVersion())
	os.Exit(exitUpdateFound)
}

func usage() {
	fmt.Fprintf(os.Stderr, `gaze %s — a system monitor for Linux terminals.

Usage:
  gaze [flags]

Flags:
`, buildVersion())
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
Updating:
  gaze --check-update   report whether a newer release exists
  gaze --update         download and install it, verifying the checksum

  --check-update exits 0 when this is the published version, 1 when a newer
  one exists, and 2 when it could not find out.

Keys:
  q            quit
  c m s t p n u
               sort processes by cpu, memory, swap, time, pid, name, user
  c m t i n    sort containers by cpu, memory, uptime, disk io, name
  v            cycle the dashboard, split, and container views
  1            toggle per-core gauges
  K            show or hide kernel threads
  /            filter processes
  space        pause
  + -          change the refresh interval
  ?            help
`)
}

// checkDir reports whether a path is a readable directory, with a message that
// names the flag to fix it.
func checkDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
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
	fatalCode(1, format, args...)
}

func fatalCode(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gaze: "+format+"\n", args...)
	os.Exit(code)
}
