package metrics

import (
	"io"
	"strings"
)

// sectorSize is the unit of the sector counts in /proc/diskstats. The kernel
// reports those in 512-byte units for every device, whatever the hardware's
// own sector size, so this is a constant and not something to look up.
const sectorSize = 512

// diskCounters holds one block device's cumulative counters.
type diskCounters struct {
	readOps, writeOps         uint64
	readSectors, writeSectors uint64
}

// used reports whether the device has ever done any I/O.
//
// A Linux host carries a fixed set of loop and network block devices that
// exist whether or not anything is attached to them. On a container host that
// is two dozen entries, all reading zero, which is enough to push every real
// device off the screen. A device whose counters have never moved has never
// been used, and nothing is lost by leaving it out.
func (d diskCounters) used() bool {
	return d.readOps+d.writeOps+d.readSectors+d.writeSectors > 0
}

// parseDiskstats reads /proc/diskstats, keyed by device name.
//
// Fields past the fourteenth were added for discard and flush operations in
// later kernels. Only the first fourteen are read, so the parser works
// unchanged across versions.
func parseDiskstats(r io.Reader) (map[string]diskCounters, error) {
	out := make(map[string]diskCounters)
	err := scanLines(r, func(line string) error {
		f := strings.Fields(line)
		if len(f) < 14 {
			return nil
		}
		out[f[2]] = diskCounters{
			readOps:      atou(f[3]),
			readSectors:  atou(f[5]),
			writeOps:     atou(f[7]),
			writeSectors: atou(f[9]),
		}
		return nil
	})
	return out, err
}
