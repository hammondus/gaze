//go:build !linux

package metrics

import "errors"

// statfsUsage is unavailable off Linux. The parsers in this package are
// portable so they can be tested anywhere, but the syscalls are not, and a
// stub keeps `go test` working on a development machine.
func statfsUsage(path string) (total, used, avail, inodesUsed, inodesFree uint64, err error) {
	err = errors.New("filesystem usage is only available on Linux")
	return
}
