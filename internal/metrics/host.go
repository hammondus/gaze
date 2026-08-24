package metrics

import (
	"io"
	"strings"
	"time"
)

// parseLoadavg reads /proc/loadavg, whose single line is
// "0.42 0.31 0.28 2/1183 28371".
func parseLoadavg(r io.Reader) (Load, error) {
	var l Load
	b, err := io.ReadAll(r)
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(b))
	if len(f) < 4 {
		return l, nil
	}
	l.One, l.Five, l.Fifteen = atof(f[0]), atof(f[1]), atof(f[2])
	if run, tot, ok := strings.Cut(f[3], "/"); ok {
		l.Runnable, l.Total = atoi(run), atoi(tot)
	}
	return l, nil
}

// parseUptime reads /proc/uptime, whose first field is seconds since boot.
func parseUptime(r io.Reader) (time.Duration, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, nil
	}
	return time.Duration(atof(f[0]) * float64(time.Second)), nil
}

// parseKernel reads /proc/sys/kernel/osrelease, giving the same string as
// "uname -r".
func parseKernel(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
