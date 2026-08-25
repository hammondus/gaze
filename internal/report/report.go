// Package report defines the wire contract between gaze-agent and
// gaze-server: the Report an agent posts, and the Directive the server
// answers with.
//
// Report is a separate type from metrics.Snapshot, and the reduction between
// them is one-way and lossy on purpose — see "The wire format is not the
// snapshot" in DESIGN-DECISIONS.md. Snapshot stays free to churn with the
// display; this package is a contract with agents already installed and with
// rows already in a database, so a shipped field is never renamed and is
// removed only by a schema bump. The golden fixture in report_test.go makes
// an accidental rename fail loudly.
package report

import "time"

// Schema is the version of the wire format this package marshals. A server
// must accept an older schema than its own, and stores what it knows of a
// newer one: an agent updates when it is told to, not when the server does.
const Schema = 1

// Stat is one figure observed across the samples of a report. Carrying the
// envelope beside the mean is what keeps a 20-second spike visible after
// aggregation — see "Sample often, report rarely" in DESIGN-DECISIONS.md.
type Stat struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// Report is one posting from an agent: the reduction of the samples it
// collected since the last one.
type Report struct {
	Schema int `json:"schema"`

	// Generation echoes the configuration generation of the last Directive
	// the agent applied, which is how the server tells a change took effect.
	Generation int `json:"generation,omitempty"`

	// Version is the agent's own build version.
	Version string `json:"version,omitempty"`

	Host Host `json:"host"`

	// Start and End span the samples' collection times, on the agent's
	// clock. A backlogged report describes the time it covers, not the time
	// it arrives — see "Reports carry the agent's clock" in
	// DESIGN-DECISIONS.md.
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Samples int       `json:"samples"`

	CPU    Stat  `json:"cpu"` // busy percent, all cores combined
	Load1  Stat  `json:"load1"`
	Load5  Stat  `json:"load5"`
	Load15 Stat  `json:"load15"`
	Memory Gauge `json:"memory"`
	Swap   Gauge `json:"swap"`

	Networks []Network `json:"networks,omitempty"`
	Disks    []Disk    `json:"disks,omitempty"`
	Mounts   []Mount   `json:"mounts,omitempty"`

	Procs ProcCounts `json:"procs"`

	// Top holds the busiest processes: the top few by processor time and,
	// separately, by resident memory, deduplicated.
	Top []Process `json:"top,omitempty"`

	// ContainerRuntime and ContainersDisabled travel with the container
	// list because "switched off", "no runtime found", and "none running"
	// are three different facts, and a server must not store one as another.
	ContainerRuntime   string      `json:"container_runtime,omitempty"`
	ContainersDisabled bool        `json:"containers_disabled,omitempty"`
	Containers         []Container `json:"containers,omitempty"`

	// Absent names the fields the host's platform could not supply, using
	// metrics.Field values. A server stores NULL for them, never zero.
	Absent []string `json:"absent,omitempty"`
}

// Host identifies the reporting machine.
type Host struct {
	Hostname      string `json:"hostname"`
	Kernel        string `json:"kernel,omitempty"`
	CPUCount      int    `json:"cpus"`
	UptimeSeconds int64  `json:"uptime_s"`
}

// Gauge is a capacity and how much of it was in use across the samples.
type Gauge struct {
	Total uint64 `json:"total"`
	Used  Stat   `json:"used"` // bytes
}

// Network is one interface's traffic over the report's span.
type Network struct {
	Name string `json:"name"`
	Rx   Stat   `json:"rx"` // bytes per second
	Tx   Stat   `json:"tx"`
	Up   bool   `json:"up"`
}

// Disk is one block device's I/O over the report's span.
type Disk struct {
	Name     string `json:"name"`
	Read     Stat   `json:"read"` // bytes per second
	Write    Stat   `json:"write"`
	ReadOps  Stat   `json:"read_ops"` // operations per second
	WriteOps Stat   `json:"write_ops"`
}

// Mount is one filesystem's usage, as last observed. Capacity moves slowly
// against a report interval, so a mount carries its latest reading rather
// than three copies of nearly the same number.
type Mount struct {
	Device  string  `json:"device"`
	Path    string  `json:"path"`
	FSType  string  `json:"fstype"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

// ProcCounts summarises the process table, as last observed.
type ProcCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running,omitempty"`
	Sleeping int `json:"sleeping,omitempty"`
	Stopped  int `json:"stopped,omitempty"`
	Zombie   int `json:"zombie,omitempty"`
	Threads  int `json:"threads,omitempty"`
	Kernel   int `json:"kernel,omitempty"`
}

// Process is one of the busiest processes. CPU is aggregated across the
// samples; the identity fields and sizes are the latest observation.
type Process struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	User    string `json:"user,omitempty"`
	CPU     Stat   `json:"cpu"`
	RSS     uint64 `json:"rss"`
	Swap    uint64 `json:"swap,omitempty"`
	Cmdline string `json:"cmdline,omitempty"`
}

// Container is one container's resource use over the report's span. A
// container that is not running keeps its identity fields and reports zero
// use, which for it is the truth.
type Container struct {
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
	State    string `json:"state"`
	CPU      Stat   `json:"cpu"`
	Mem      uint64 `json:"mem,omitempty"`
	MemLimit uint64 `json:"mem_limit,omitempty"`
}

// Directive is the server's answer to a posted report. The server never
// opens a connection to an agent; anything it wants changed rides here, and
// the agent applies what its start-up flags allow — see "The agent is told
// what to do in the reply" in DESIGN-DECISIONS.md. Process command-line
// collection is deliberately not here: it is a local, agent-only choice, and
// no directive field may ever switch it on.
type Directive struct {
	// Generation numbers the configuration this directive carries. The
	// agent echoes it back in its next report, which is how the server
	// learns a change took effect rather than hoping it did.
	Generation int `json:"generation"`

	// SampleSeconds and ReportSeconds set the agent's two intervals. Zero
	// leaves the current value alone.
	SampleSeconds int `json:"sample_s,omitempty"`
	ReportSeconds int `json:"report_s,omitempty"`

	// Containers switches container collection. Nil leaves it alone.
	Containers *bool `json:"containers,omitempty"`

	// Update tells the agent to run its self-update path. It is a bare
	// trigger by design: it carries no version and no URL, so a server can
	// make an agent fetch the latest release sooner but can never hand it a
	// binary or pin it to an old one. No future field may change that.
	Update bool `json:"update,omitempty"`
}
