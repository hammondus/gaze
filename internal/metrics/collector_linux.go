package metrics

import "os"

// NewSource returns a Source reading the running kernel.
//
// This file and collector_unsupported.go are the platform boundary: the one
// place that knows how to observe the machine the program is running on.
// Snapshot is the platform-neutral contract on the other side of it, and the
// rest of the package reads from any fs.FS, so only this constructor needs a
// build constraint.
func NewSource() Source {
	return Source{
		Proc: os.DirFS("/proc"),
		Sys:  os.DirFS("/sys"),
	}
}
