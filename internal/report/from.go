package report

import (
	"math"
	"slices"
	"sort"

	"github.com/hammondus/gaze/internal/metrics"
)

// Options configure the reduction. The zero value is what an agent ships
// with.
type Options struct {
	// Cmdlines includes each reported process's command line. Off by
	// default: a command line can carry secrets typed onto it, and sending
	// those is a consent the host's operator gives locally — a Directive
	// cannot switch it on.
	Cmdlines bool

	// TopProcesses is how many processes to report by processor time and,
	// separately, by resident memory; the two sets are sent deduplicated.
	// Zero means 5.
	TopProcesses int
}

const defaultTopProcesses = 5

// From reduces a run of consecutive snapshots to one Report.
//
// It takes the sample slice rather than folding finished reports together:
// minimum, maximum, and mean need the series, and the busiest processes can
// only be ranked fairly by matching PIDs across samples. An empty slice
// returns the zero Report.
//
// A name missing from one sample — an interface that appeared, a process
// that exited — counts as zero for the samples it missed, because zero is
// what it contributed then.
func From(samples []metrics.Snapshot, o Options) Report {
	if len(samples) == 0 {
		return Report{}
	}
	topN := o.TopProcesses
	if topN <= 0 {
		topN = defaultTopProcesses
	}
	last := samples[len(samples)-1]

	r := Report{
		Schema: Schema,
		Host: Host{
			Hostname:      last.Host.Hostname,
			Kernel:        last.Host.Kernel,
			CPUCount:      last.Host.CPUCount,
			UptimeSeconds: int64(last.Host.Uptime.Seconds()),
		},
		Start:   samples[0].Taken,
		End:     last.Taken,
		Samples: len(samples),

		CPU:    statOver(samples, func(s metrics.Snapshot) float64 { return s.CPU.Busy }),
		Load1:  statOver(samples, func(s metrics.Snapshot) float64 { return s.Load.One }),
		Load5:  statOver(samples, func(s metrics.Snapshot) float64 { return s.Load.Five }),
		Load15: statOver(samples, func(s metrics.Snapshot) float64 { return s.Load.Fifteen }),
		Memory: Gauge{
			Total: last.Memory.Total,
			Used:  statOver(samples, func(s metrics.Snapshot) float64 { return float64(s.Memory.Used) }),
		},
		Swap: Gauge{
			Total: last.Swap.Total,
			Used:  statOver(samples, func(s metrics.Snapshot) float64 { return float64(s.Swap.Used) }),
		},

		Procs: ProcCounts{
			Total:    last.ProcCounts.Total,
			Running:  last.ProcCounts.Running,
			Sleeping: last.ProcCounts.Sleeping,
			Stopped:  last.ProcCounts.Stopped,
			Zombie:   last.ProcCounts.Zombie,
			Threads:  last.ProcCounts.Threads,
			Kernel:   last.ProcCounts.Kernel,
		},

		ContainerRuntime:   last.ContainerRuntime,
		ContainersDisabled: last.ContainersDisabled,
	}

	r.Networks = reduceNetworks(samples)
	r.Disks = reduceDisks(samples)
	r.Mounts = reduceMounts(last)
	r.Top = reduceProcesses(samples, topN, o.Cmdlines)
	r.Containers = reduceContainers(samples)
	r.Absent = reduceAbsent(samples)
	return r
}

// reduceNetworks aggregates each interface's rates, in first-seen order.
func reduceNetworks(samples []metrics.Snapshot) []Network {
	var names []string
	latest := map[string]metrics.Network{}
	rx := map[string][]float64{}
	tx := map[string][]float64{}
	for i, s := range samples {
		for _, n := range s.Networks {
			if _, ok := latest[n.Name]; !ok {
				names = append(names, n.Name)
			}
			latest[n.Name] = n
			rx[n.Name] = padTo(rx[n.Name], i)
			tx[n.Name] = padTo(tx[n.Name], i)
			rx[n.Name] = append(rx[n.Name], n.RxRate)
			tx[n.Name] = append(tx[n.Name], n.TxRate)
		}
	}
	out := make([]Network, 0, len(names))
	for _, name := range names {
		out = append(out, Network{
			Name: name,
			Rx:   reduce(padTo(rx[name], len(samples))),
			Tx:   reduce(padTo(tx[name], len(samples))),
			Up:   latest[name].Up,
		})
	}
	return out
}

// reduceDisks aggregates each block device's rates, in first-seen order.
func reduceDisks(samples []metrics.Snapshot) []Disk {
	var names []string
	series := map[string]*[4][]float64{} // read, write, readOps, writeOps
	for i, s := range samples {
		for _, d := range s.Disks {
			sr, ok := series[d.Name]
			if !ok {
				names = append(names, d.Name)
				sr = &[4][]float64{}
				series[d.Name] = sr
			}
			for j, v := range [4]float64{d.ReadRate, d.WriteRate, d.IOPSRead, d.IOPSWrite} {
				sr[j] = append(padTo(sr[j], i), v)
			}
		}
	}
	out := make([]Disk, 0, len(names))
	for _, name := range names {
		sr := series[name]
		out = append(out, Disk{
			Name:     name,
			Read:     reduce(padTo(sr[0], len(samples))),
			Write:    reduce(padTo(sr[1], len(samples))),
			ReadOps:  reduce(padTo(sr[2], len(samples))),
			WriteOps: reduce(padTo(sr[3], len(samples))),
		})
	}
	return out
}

// reduceMounts carries the last observation of each mount. Capacity moves
// slowly against a report interval, so an envelope would be three copies of
// nearly the same number.
func reduceMounts(last metrics.Snapshot) []Mount {
	out := make([]Mount, 0, len(last.Mounts))
	for _, m := range last.Mounts {
		out = append(out, Mount{
			Device:  m.Device,
			Path:    m.Path,
			FSType:  m.FSType,
			Total:   m.Total,
			Used:    m.Used,
			Percent: round2(m.Percent),
		})
	}
	return out
}

// reduceProcesses matches processes by PID across the samples, aggregates
// their CPU, and keeps the top few by mean CPU and, separately, by resident
// memory. Identity fields and sizes come from each process's latest
// appearance.
//
// A recycled PID within one report span would fold two processes into one
// row. The span is about a minute, the identity fields come from the later
// process, and the CPU envelope stays truthful about what that PID cost, so
// the confusion is bounded and not worth carrying kernel start times to
// prevent.
func reduceProcesses(samples []metrics.Snapshot, topN int, cmdlines bool) []Process {
	latest := map[int]metrics.Process{}
	cpu := map[int][]float64{}
	for i, s := range samples {
		for _, p := range s.Processes {
			latest[p.PID] = p
			cpu[p.PID] = append(padTo(cpu[p.PID], i), p.CPU)
		}
	}

	pids := make([]int, 0, len(latest))
	for pid := range latest {
		pids = append(pids, pid)
	}
	stats := map[int]Stat{}
	for _, pid := range pids {
		stats[pid] = reduce(padTo(cpu[pid], len(samples)))
	}

	byCPU := slices.Clone(pids)
	sort.Slice(byCPU, func(i, j int) bool {
		a, b := stats[byCPU[i]], stats[byCPU[j]]
		if a.Mean != b.Mean {
			return a.Mean > b.Mean
		}
		return byCPU[i] < byCPU[j]
	})
	byRSS := slices.Clone(pids)
	sort.Slice(byRSS, func(i, j int) bool {
		a, b := latest[byRSS[i]], latest[byRSS[j]]
		if a.RSS != b.RSS {
			return a.RSS > b.RSS
		}
		return byRSS[i] < byRSS[j]
	})

	keep := byCPU[:min(topN, len(byCPU))]
	for _, pid := range byRSS[:min(topN, len(byRSS))] {
		if !slices.Contains(keep, pid) {
			keep = append(keep, pid)
		}
	}

	out := make([]Process, 0, len(keep))
	for _, pid := range keep {
		p := latest[pid]
		rp := Process{
			PID:  p.PID,
			Name: p.Name,
			User: p.User,
			CPU:  stats[pid],
			RSS:  p.RSS,
			Swap: p.Swap,
		}
		if cmdlines {
			rp.Cmdline = p.Cmdline
		}
		out = append(out, rp)
	}
	return out
}

// reduceContainers aggregates each container's CPU by name, in first-seen
// order. Identity, state, and memory come from the latest appearance.
func reduceContainers(samples []metrics.Snapshot) []Container {
	var names []string
	latest := map[string]metrics.Container{}
	cpu := map[string][]float64{}
	for i, s := range samples {
		for _, c := range s.Containers {
			if _, ok := latest[c.Name]; !ok {
				names = append(names, c.Name)
			}
			latest[c.Name] = c
			cpu[c.Name] = append(padTo(cpu[c.Name], i), c.CPU)
		}
	}
	out := make([]Container, 0, len(names))
	for _, name := range names {
		c := latest[name]
		out = append(out, Container{
			Name:     c.Name,
			Image:    c.Image,
			State:    c.State,
			CPU:      reduce(padTo(cpu[name], len(samples))),
			Mem:      c.MemUsed,
			MemLimit: c.MemLimit,
		})
	}
	return out
}

// reduceAbsent unions the samples' absent fields, sorted for determinism.
func reduceAbsent(samples []metrics.Snapshot) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range samples {
		for _, f := range s.Absent {
			if !seen[string(f)] {
				seen[string(f)] = true
				out = append(out, string(f))
			}
		}
	}
	sort.Strings(out)
	return out
}

// statOver reduces one field read from every sample.
func statOver(samples []metrics.Snapshot, f func(metrics.Snapshot) float64) Stat {
	vs := make([]float64, len(samples))
	for i, s := range samples {
		vs[i] = f(s)
	}
	return reduce(vs)
}

// reduce turns a series into its envelope and mean, rounded to two decimals:
// the wire does not need micro-percent, and a full float64 doubles the bytes
// of every figure it carries.
func reduce(vs []float64) Stat {
	if len(vs) == 0 {
		return Stat{}
	}
	st := Stat{Min: vs[0], Max: vs[0]}
	var sum float64
	for _, v := range vs {
		st.Min = math.Min(st.Min, v)
		st.Max = math.Max(st.Max, v)
		sum += v
	}
	st.Min = round2(st.Min)
	st.Max = round2(st.Max)
	st.Mean = round2(sum / float64(len(vs)))
	return st
}

// padTo extends a series with zeros up to length n, for a name that was
// missing from earlier samples: zero is what it contributed then.
func padTo(vs []float64, n int) []float64 {
	for len(vs) < n {
		vs = append(vs, 0)
	}
	return vs
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
