package metrics

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// UpdatesFile is the name, inside Source.State, of the counts the apt hook
// writes. The hook is contrib/gaze-apt-updates; apt runs it as root after
// every package-list refresh and every install, so the agent itself never
// needs root, never execs, and never solves dependencies.
const UpdatesFile = "updates"

// Updates is the apt hook's count of pending upgrades.
type Updates struct {
	// Upgradable is every package `apt upgrade` would install now, and
	// Security the subset whose candidate comes from a security archive.
	Upgradable int
	Security   int

	// Counted is when the hook last wrote the file, on this host's clock.
	// The count is only as fresh as the package lists it was taken from, so
	// a consumer must show its age: "0 updates" from three weeks ago is not
	// good news.
	Counted time.Time
}

// readUpdates reads the apt hook's counts. A nil filesystem or a missing
// file is no count and no error: the hook is an opt-in install step, and a
// host without it must read as "not counted", never as "up to date".
func readUpdates(fsys fs.FS) (*Updates, error) {
	if fsys == nil {
		return nil, nil
	}
	f, err := fsys.Open(UpdatesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	u, err := parseUpdates(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", UpdatesFile, err)
	}
	u.Counted = fi.ModTime()
	return &u, nil
}

// parseUpdates reads the hook's key=value lines. Unknown keys are ignored,
// so a later hook can add one without breaking an older agent; both known
// keys are required, because a count that is half there is not a count.
func parseUpdates(r io.Reader) (Updates, error) {
	var u Updates
	var haveAll, haveSec bool
	err := scanLines(r, func(line string) error {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		switch k {
		case "upgradable":
			u.Upgradable, haveAll = n, err == nil
		case "security":
			u.Security, haveSec = n, err == nil
		default:
			return nil
		}
		if err != nil || n < 0 {
			return fmt.Errorf("bad %s value %q", k, v)
		}
		return nil
	})
	if err != nil {
		return Updates{}, err
	}
	if !haveAll || !haveSec {
		return Updates{}, errors.New("want both upgradable= and security=")
	}
	return u, nil
}
