package metrics

import (
	"io"
	"strings"
)

// parseMeminfo reads /proc/meminfo into Memory and Swap.
//
// Every value in that file is in kibibytes, despite the "kB" suffix, so each
// is shifted left by ten to reach bytes.
func parseMeminfo(r io.Reader) (Memory, Swap, error) {
	var m Memory
	var s Swap
	var sReclaimable uint64
	haveAvailable := false

	err := scanLines(r, func(line string) error {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		v := atou(strings.Fields(val)[0]) << 10
		switch key {
		case "MemTotal":
			m.Total = v
		case "MemFree":
			m.Free = v
		case "MemAvailable":
			m.Available = v
			haveAvailable = true
		case "Buffers":
			m.Buffers = v
		case "Cached":
			m.Cached = v
		case "SReclaimable":
			sReclaimable = v
		case "SwapTotal":
			s.Total = v
		case "SwapFree":
			s.Free = v
		}
		return nil
	})
	if err != nil {
		return m, s, err
	}

	// MemAvailable arrived in Linux 3.14. On anything older, approximate it
	// the way the kernel patch did: free memory plus the page cache and the
	// reclaimable slab.
	if !haveAvailable {
		m.Available = m.Free + m.Buffers + m.Cached + sReclaimable
	}
	if m.Available > m.Total {
		m.Available = m.Total
	}
	m.Used = m.Total - m.Available
	m.Percent = pct(m.Used, m.Total)

	s.Used = s.Total - s.Free
	s.Percent = pct(s.Used, s.Total)
	return m, s, nil
}
