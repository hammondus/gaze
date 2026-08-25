package metrics

import (
	"bufio"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

// Source is where a collector reads from. Production code uses NewSource,
// which points at the real /proc and /sys. Tests point at fixture directories,
// which is why every parser below takes a Source or a reader rather than a
// hard-coded path.
//
// NewSource is the platform boundary, defined in collector_linux.go and
// collector_unsupported.go. Everything else in this package is free of build
// constraints, so the fixture tests run on any platform.
type Source struct {
	Proc fs.FS
	Sys  fs.FS
}

// open returns a reader for a path relative to the given filesystem. Callers
// close it.
func open(fsys fs.FS, name string) (fs.File, error) {
	return fsys.Open(name)
}

// readFile reads a whole pseudo-file. Files under /proc and /sys report a size
// of zero, so a size-hinted read is useless here and io.ReadAll is the only
// correct approach.
func readFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// readTrimmed reads a pseudo-file holding a single value and strips the
// trailing newline. Most files under /sys are of this shape.
func readTrimmed(fsys fs.FS, name string) (string, error) {
	b, err := readFile(fsys, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readUint reads a pseudo-file holding a single unsigned integer.
func readUint(fsys fs.FS, name string) (uint64, error) {
	s, err := readTrimmed(fsys, name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

// scanLines calls fn for each line of r, stopping at the first error fn
// returns. It exists so the parsers below share one buffered-read path rather
// than each allocating their own scanner and buffer.
func scanLines(r io.Reader, fn func(line string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := fn(sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// atou parses an unsigned integer, returning zero for anything unparseable.
// Kernel counters are well-formed by construction; a malformed field means the
// format has changed under you, and dropping that one value beats failing the
// whole collection.
func atou(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// atoi parses a signed integer, returning zero for anything unparseable.
func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// atof parses a float, returning zero for anything unparseable.
func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// pct returns part as a percentage of whole, and zero when whole is zero.
func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

// rate converts two readings of a monotonic counter into a per-second rate.
//
// A counter that moved backwards means the device was reset, unplugged, or
// renumbered. There is no way to know how much it counted before that, so the
// rate is zero for one interval rather than a spike of nonsense.
func rate(prev, cur uint64, seconds float64) float64 {
	if seconds <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / seconds
}
