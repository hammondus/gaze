package metrics

import (
	"io"
	"strings"
)

// netCounters holds one interface's cumulative counters from /proc/net/dev.
type netCounters struct {
	rxBytes, rxPackets, rxErrs, rxDrops uint64
	txBytes, txPackets, txErrs, txDrops uint64
}

// parseNetDev reads /proc/net/dev, keyed by interface name.
//
// The file's two header lines are skipped by requiring a colon, which only
// data lines carry after the interface name. An interface name is not padded
// to a fixed width and can run up against the colon, so the split is on the
// colon rather than on whitespace.
func parseNetDev(r io.Reader) (map[string]netCounters, error) {
	out := make(map[string]netCounters)
	err := scanLines(r, func(line string) error {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if len(f) < 16 {
			return nil
		}
		out[name] = netCounters{
			rxBytes: atou(f[0]), rxPackets: atou(f[1]),
			rxErrs: atou(f[2]), rxDrops: atou(f[3]),
			txBytes: atou(f[8]), txPackets: atou(f[9]),
			txErrs: atou(f[10]), txDrops: atou(f[11]),
		}
		return nil
	})
	return out, err
}
