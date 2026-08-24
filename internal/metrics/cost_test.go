package metrics

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

// cpuUsed returns the CPU this process has consumed, user plus system.
//
// Wall-clock timing is the wrong instrument for this question. Most of a
// container poll is spent blocked on the daemon socket, which costs latency
// but almost no CPU, so an elapsed-time measurement reports the socket's
// round-trip and calls it work.
func cpuUsed() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestCollectCost reports what one collection costs, with and without
// container polling. It answers "where does gaze's CPU go" with a number
// rather than a guess.
//
// It needs a real kernel, and containers running to be worth anything:
//
//	GAZE_LIVE=1 go test ./internal/metrics -run TestCollectCost -v
//
// Both CPU and wall time are reported because they answer different
// questions. CPU is what shows up when comparing against another monitor.
// Wall time is how long the collection goroutine is in flight, which matters
// only if it approaches the refresh interval.
func TestCollectCost(t *testing.T) {
	if os.Getenv("GAZE_LIVE") == "" {
		t.Skip("set GAZE_LIVE=1 to measure against the running kernel")
	}
	const rounds = 30

	var withCPU, withoutCPU float64
	for _, c := range []struct {
		label string
		opts  Options
	}{
		{"containers on ", Options{}},
		{"containers off", Options{DisableContainers: true}},
	} {
		col := New(c.opts)
		ctx := context.Background()
		col.Collect(ctx) // warm up: the first collection primes nothing else does

		cpu0, wall0 := cpuUsed(), time.Now()
		var procs, ctrs int
		for i := 0; i < rounds; i++ {
			s := col.Collect(ctx)
			procs, ctrs = len(s.Processes), len(s.Containers)
		}
		cpuPer := float64((cpuUsed() - cpu0).Microseconds()) / 1000 / rounds
		wallPer := float64(time.Since(wall0).Microseconds()) / 1000 / rounds

		if c.opts.DisableContainers {
			withoutCPU = cpuPer
		} else {
			withCPU = cpuPer
		}
		t.Logf("%s  %3d processes %2d containers | CPU %6.2f ms | wall %6.2f ms",
			c.label, procs, ctrs, cpuPer, wallPer)
	}

	if withCPU < withoutCPU {
		t.Errorf("collecting containers measured cheaper than skipping them: %.2f < %.2f",
			withCPU, withoutCPU)
	}
	if withoutCPU > 0 {
		t.Log(fmt.Sprintf("container polling costs %.2f ms of CPU per collection (%.0f%% of the total)",
			withCPU-withoutCPU, (withCPU-withoutCPU)/withCPU*100))
	}
}
