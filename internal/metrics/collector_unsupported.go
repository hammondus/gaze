//go:build !linux

package metrics

import (
	"fmt"
	"io/fs"
	"runtime"
)

// NewSource returns a Source whose every read fails, naming the platform.
//
// Collection from the running system is implemented for Linux only. On any
// other platform a collector still constructs and runs — the failures land in
// Snapshot.Errs, per "errors do not clear the screen" — but every reading it
// would take is refused here, in one place, rather than by a missing /proc.
// A future platform gets its own collector_<goos>.go implementing NewSource
// for real, and reports what it cannot supply through Snapshot.Absent.
func NewSource() Source {
	return Source{
		Proc: unsupportedFS{},
		Sys:  unsupportedFS{},
	}
}

// unsupportedFS refuses every open with an error naming the platform.
type unsupportedFS struct{}

func (unsupportedFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{
		Op:   "open",
		Path: name,
		Err:  fmt.Errorf("collection from the running system is implemented for Linux only; this is %s", runtime.GOOS),
	}
}
