package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/alert"
	"github.com/hammondus/gaze/internal/devices"
	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/gaze/internal/threshold"
)

// staleAfter is how long after its last report a host is drawn as stale:
// several missed reports at the default interval, so a slow network or one
// lost POST does not flap the list. It is the alert package's constant, so
// the pages, the SSH list, and the staleness mail can never disagree about
// what "stale" means. Staleness reads receive time, so a host with a
// broken clock still goes stale.
const staleAfter = alert.StaleAfter

// hostState is the three-way fact the fleet list exists to show. The done-
// when for this stage: never reported, reporting, and stopped must each be
// unmistakable at a glance.
func hostState(lastSeen time.Time) (label, class string) {
	switch {
	case lastSeen.IsZero():
		return "never reported", "never"
	case time.Since(lastSeen) < staleAfter:
		return "reporting", "ok"
	default:
		return "stale", "stale"
	}
}

// updatesOldAfter is when an update count stops being news. The daily apt
// timer refreshes the package lists, and the hook recounts on every
// refresh, so a count three days old means the refresh has stopped — and
// "0 updates" from a list nobody has refreshed is not good news.
const updatesOldAfter = 72 * time.Hour

// levelClass maps a threshold level onto the stylesheet's colour classes.
// Red is the alert threshold, the same as in the TUI and the alert mail.
func levelClass(l threshold.Level) string {
	switch l {
	case threshold.Crit:
		return "bad"
	case threshold.Warn:
		return "warn"
	}
	return ""
}

// fleetView is the host list page: the rows, and the time they were read,
// so a reload is visible.
type fleetView struct {
	Now  time.Time
	Rows []fleetRow

	// Notice answers the update-all button: how many agents it asked.
	// Held is set when any request is held, and says why once for the
	// whole list rather than in every row's hover text.
	Notice string
	Held   string
}

// fleetRow is one host on the list.
type fleetRow struct {
	query.Overview
	State      string
	StateClass string
	MemPct     float64
	MemClass   string

	// The fullest filesystem, and every filesystem for the hover text.
	// DiskPath is empty when the latest report carried no mounts.
	DiskPath  string
	DiskPct   float64
	DiskClass string
	DiskTitle string

	// UpdatesOld marks a count taken more than updatesOldAfter ago.
	UpdatesOld bool

	// Update is where a requested self-update stands.
	Update updateView

	// The remote-configuration standing. A declined directive must be as
	// visible here as an applied one.
	CfgStatus string
	CfgClass  string
}

func (s *webServer) handleFleet(w http.ResponseWriter, r *http.Request) {
	fleet, err := s.q.Fleet(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The gate is consulted only while some update stands requested: it
	// is the same hourly-cached lookup the ingest path makes, and a host
	// list with nothing pending has no reason to ask GitHub anything.
	var latest string
	var sendable bool
	for _, o := range fleet {
		if !o.UpdateAsked.IsZero() {
			latest, sendable = s.gate.check()
			break
		}
	}

	view := fleetView{Now: time.Now()}
	rows := make([]fleetRow, 0, len(fleet))
	for _, o := range fleet {
		row := fleetRow{Overview: o}
		row.State, row.StateClass = hostState(o.LastSeen)
		if o.MemTotal > 0 {
			row.MemPct = o.MemUsed / float64(o.MemTotal) * 100
			row.MemClass = levelClass(threshold.Memory.Of(row.MemPct))
		}
		if len(o.Mounts) > 0 {
			// Fleet sorts mounts fullest first.
			row.DiskPath, row.DiskPct = o.Labels.Of(store.LabelMount, o.Mounts[0].Path), o.Mounts[0].Percent
			row.DiskClass = levelClass(threshold.Disk.Of(row.DiskPct))
			lines := make([]string, len(o.Mounts))
			for i, m := range o.Mounts {
				lines[i] = fmt.Sprintf("%s %s of %s", fmtPercent(m.Percent), m.Path, fmtBytes(m.Total))
			}
			row.DiskTitle = strings.Join(lines, "\n")
		}
		if o.Updates != nil {
			row.UpdatesOld = time.Since(o.Updates.Counted) > updatesOldAfter
		}
		row.CfgStatus, row.CfgClass = configStatus(o.Generation, o.CfgGeneration, o.Declined)
		row.Update = updateStanding(updateFacts{
			Asked: o.UpdateAsked, Sent: o.UpdateSent, LastSeen: o.LastSeen,
			AgentVersion: o.AgentVersion, Declined: o.Declined, UpdateError: o.UpdateError,
			Slot: updateSlot(o.ID),
		}, latest, s.gate.serverVersion(), sendable)
		if row.Update.Label == "held" {
			view.Held = strings.TrimPrefix(row.Update.Detail, "Held: ")
		}
		rows = append(rows, row)
	}
	view.Rows = rows
	if asked := r.URL.Query().Get("asked"); asked != "" {
		view.Notice = askedNotice(asked, latest)
	}
	// The host list reloads on the agents' default report interval, so a
	// host going stale shows without a manual refresh. The page is a
	// glance; graphs and forms elsewhere keep the manual reload.
	// The reload goes to the bare list, so the notice from the update-all
	// button shows once rather than on every reload after it.
	s.render(w, r, "fleet", page{Title: "Hosts", Authed: true, Refresh: 60, RefreshTo: "/", Data: view})
}

// askedNotice is the line the update-all button leaves on the host list.
func askedNotice(asked, latest string) string {
	n, err := strconv.Atoi(asked)
	switch {
	case err != nil || n < 0:
		return ""
	case n == 0 && latest != "":
		return "Every agent that has reported is already on " + latest + "."
	case n == 0:
		return "No agent has reported yet, so there is nothing to update."
	}
	agents := "agents"
	if n == 1 {
		agents = "agent"
	}
	to := ""
	if latest != "" {
		to = " to " + latest
	}
	return fmt.Sprintf("Asked %d %s to update%s. Each is sent the update at its own time within %d minutes, so the fleet does not download at once.",
		n, agents, to, int(staggerWindow.Minutes()))
}

// ranges are the spans the host page offers. An ordered slice, not a map:
// the links render in this order.
var ranges = []struct {
	Key  string
	Span time.Duration
}{
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
	{"1y", 365 * 24 * time.Hour},
}

type rangeLink struct {
	Key    string
	Href   string
	Active bool
}

// hostHref builds a link to the host page. Every link on the page goes through
// it, so a range link keeps the device setting and the device link keeps the
// range: changing one control must not silently reset the other.
func hostHref(id int64, rangeKey string, virtual bool) string {
	q := url.Values{"range": {rangeKey}}
	if virtual {
		q.Set("virtual", "1")
	}
	return fmt.Sprintf("/hosts/%d?%s", id, q.Encode())
}

// hostView is everything the detail page shows.
type hostView struct {
	query.Overview
	State      string
	StateClass string
	Range      string
	Ranges     []rangeLink

	// Latest is the newest stored report, for the tables; nil when the
	// host has never reported.
	Latest *report.Report

	// Cfg is the remote-management standing, and CfgStatus/CfgClass the
	// one-line summary of whether the agent has taken it.
	Cfg       store.HostConfig
	CfgStatus string
	CfgClass  string
	Update    updateView

	Graphs []graph // cpu, load, memory, swap
	Nets   []graph // one per interface
	Disks  []graph // one per device

	// The virtual devices are off the page unless ShowVirtual is set: a
	// container host has one veth and one loop device per container, and
	// each of them is a graph the size of the one for the real NIC.
	// DeviceLabel is the toggle's text, empty when there is nothing to
	// toggle, and DeviceHref the link that flips it.
	ShowVirtual bool
	DeviceLabel string
	DeviceHref  string

	// Labelled is every mount, interface, and block device the page shows,
	// with its current label if any: the rows of the labels form.
	Labelled []labelRow
}

// labelRow is one line of the labels form.
type labelRow struct {
	Kind  string // store.LabelMount, LabelNet, or LabelDisk: the field name
	What  string // the word the page uses: mount, interface, disk
	Name  string // as reported
	Label string // current label, empty for none
}

// captioned names a thing for a graph caption: the label with the
// reported name after it, or the reported name alone. The reported name
// stays in view because the graph is where someone goes to work out which
// device is busy.
func captioned(labels query.Labels, kind, name string) string {
	if labels.Has(kind, name) {
		return labels.Of(kind, name) + " (" + name + ")"
	}
	return name
}

func (s *webServer) handleHost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// The fleet query also carries the one host's identity row; at this
	// system's scale one list query is cheaper to own than a second
	// per-host lookup.
	fleet, err := s.q.Fleet(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := hostView{Range: "24h"}
	found := false
	for _, o := range fleet {
		if o.ID == id {
			v.Overview, found = o, true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	v.State, v.StateClass = hostState(v.LastSeen)

	v.Cfg, err = s.store.HostConfig(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.CfgStatus, v.CfgClass = configStatus(v.Cfg.Echoed, v.Cfg.Generation, v.Cfg.Declined)
	if !v.Cfg.UpdateAsked.IsZero() {
		latest, sendable := s.gate.check()
		v.Update = updateStanding(updateFacts{
			Asked: v.Cfg.UpdateAsked, Sent: v.Cfg.UpdateSent, LastSeen: v.LastSeen,
			AgentVersion: v.Cfg.AgentVersion, Declined: v.Cfg.Declined, UpdateError: v.Cfg.UpdateError,
			Slot: updateSlot(id),
		}, latest, s.gate.serverVersion(), sendable)
	}

	v.ShowVirtual = r.URL.Query().Get("virtual") == "1"
	if key := r.URL.Query().Get("range"); key != "" {
		for _, rg := range ranges {
			if rg.Key == key {
				v.Range = key
			}
		}
	}
	var span time.Duration
	for _, rg := range ranges {
		v.Ranges = append(v.Ranges, rangeLink{
			Key: rg.Key, Href: hostHref(id, rg.Key, v.ShowVirtual), Active: rg.Key == v.Range,
		})
		if rg.Key == v.Range {
			span = rg.Span
		}
	}
	to := time.Now()
	from := to.Add(-span)

	latest, err := s.q.Latest(r.Context(), id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Never reported: the page still renders, all gaps and dashes.
	case err != nil:
		s.fail(w, r, err)
		return
	default:
		v.Latest = latest
	}

	if v.Latest != nil {
		for _, m := range v.Latest.Mounts {
			v.Labelled = append(v.Labelled, labelRow{Kind: store.LabelMount, What: "mount", Name: m.Path,
				Label: v.Labels[query.LabelKey{Kind: store.LabelMount, Name: m.Path}]})
		}
	}

	points, err := s.q.Scalars(r.Context(), id, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.Graphs = scalarGraphs(points, from, to)

	nets, err := s.q.Nets(r.Context(), id, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hidden := 0
	for _, n := range nets {
		if !v.ShowVirtual && devices.VirtualNet(n.Name) {
			hidden++
			continue
		}
		v.Labelled = append(v.Labelled, labelRow{Kind: store.LabelNet, What: "interface", Name: n.Name,
			Label: v.Labels[query.LabelKey{Kind: store.LabelNet, Name: n.Name}]})
		v.Nets = append(v.Nets, buildGraph("net "+captioned(v.Labels, store.LabelNet, n.Name)+" — rx / tx", from, to, 0, fmtYRate, false,
			series{class: "a", points: netPoints(n.Points, false)},
			series{class: "b", points: netPoints(n.Points, true)}))
	}

	disks, err := s.q.Disks(r.Context(), id, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, d := range disks {
		if !v.ShowVirtual && devices.VirtualDisk(d.Name) {
			hidden++
			continue
		}
		v.Labelled = append(v.Labelled, labelRow{Kind: store.LabelDisk, What: "disk", Name: d.Name,
			Label: v.Labels[query.LabelKey{Kind: store.LabelDisk, Name: d.Name}]})
		v.Disks = append(v.Disks, buildGraph("disk "+captioned(v.Labels, store.LabelDisk, d.Name)+" — read / write", from, to, 0, fmtYRate, false,
			series{class: "a", points: diskPoints(d.Points, false)},
			series{class: "b", points: diskPoints(d.Points, true)}))
	}
	v.DeviceLabel, v.DeviceHref = deviceToggle(id, v.Range, v.ShowVirtual, hidden)

	s.render(w, r, "host", page{Title: v.Name, Authed: true, Data: v})
}

// scalarGraphs builds the four host-level graphs from one Scalars pass.
func scalarGraphs(points []query.Point, from, to time.Time) []graph {
	pick := func(f func(query.Point) (report.Stat, bool)) []gpoint {
		out := make([]gpoint, 0, len(points))
		for _, p := range points {
			st, ok := f(p)
			out = append(out, gpoint{
				t: p.Start, min: st.Min, max: st.Max, mean: st.Mean,
				weight: p.Samples, skip: !ok,
			})
		}
		return out
	}

	cpu := pick(func(p query.Point) (report.Stat, bool) { return p.CPU, true })
	load := pick(func(p query.Point) (report.Stat, bool) { return p.Load1, true })
	mem := pick(func(p query.Point) (report.Stat, bool) { return p.Mem, true })

	// Swap points are absent when the platform said so, and the graph is
	// mute when the host simply has none — different facts, drawn
	// differently: gaps against "no swap".
	swap := pick(func(p query.Point) (report.Stat, bool) {
		return p.Swap, !hasAbsent(p.Absent, "swap")
	})
	var memTotal, swapTotal float64
	for _, p := range points {
		memTotal = max(memTotal, float64(p.MemTotal))
		swapTotal = max(swapTotal, float64(p.SwapTotal))
	}

	gs := []graph{
		buildGraph("cpu %", from, to, 100, fmtYPercent, true, series{class: "a", points: cpu}),
		buildGraph("load (1m)", from, to, 0, fmtYLoad, true, series{class: "a", points: load}),
		buildGraph("memory used", from, to, memTotal, fmtYBytes, true, series{class: "a", points: mem}),
	}
	sg := buildGraph("swap used", from, to, swapTotal, fmtYBytes, true, series{class: "a", points: swap})
	if len(points) > 0 && swapTotal == 0 && sg.Note == "" {
		sg.Note = "no swap"
		sg.Band, sg.Lines, sg.Dots = "", nil, nil
	}
	gs = append(gs, sg)
	return gs
}

func netPoints(ps []query.NetPoint, tx bool) []gpoint {
	out := make([]gpoint, 0, len(ps))
	for _, p := range ps {
		st := p.Rx
		if tx {
			st = p.Tx
		}
		out = append(out, gpoint{t: p.Start, min: st.Min, max: st.Max, mean: st.Mean, weight: 1})
	}
	return out
}

func diskPoints(ps []query.DiskPoint, write bool) []gpoint {
	out := make([]gpoint, 0, len(ps))
	for _, p := range ps {
		st := p.Read
		if write {
			st = p.Write
		}
		out = append(out, gpoint{t: p.Start, min: st.Min, max: st.Max, mean: st.Mean, weight: 1})
	}
	return out
}

// Axis label formatters.

func fmtYPercent(v float64) string { return fmt.Sprintf("%.0f%%", v) }

func fmtYLoad(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func fmtYBytes(v float64) string {
	if v < 0 {
		v = 0
	}
	return fmtBytes(uint64(v))
}

func fmtYRate(v float64) string {
	if v < 0 {
		v = 0
	}
	return fmtRate(v)
}

// enrollData is the token page: shown once, stored nowhere.
type enrollData struct {
	Name  string
	Token string
}

func (s *webServer) handleEnrollForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "enroll", page{Title: "Enrol a host", Authed: true})
}

// handleEnroll wraps the stage-4 store path the CLI uses: mint a token,
// show it exactly once, store only its hash. Enrolling an existing host
// mints it a second token, which is how a token is rotated.
func (s *webServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.render(w, r, "enroll", page{
			Title:  "Enrol a host",
			Authed: true,
			Error:  "Enter the hostname to enrol.",
		})
		return
	}
	token, err := s.store.Enroll(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The one page that shows a secret that must outlive it: keep it off
	// every disk. no-store also kills the back/forward cache, which is the
	// point here.
	w.Header().Set("Cache-Control", "no-store, private")
	s.render(w, r, "enrolled", page{
		Title:  "Host enrolled",
		Authed: true,
		Data:   enrollData{Name: name, Token: token},
	})
}

// deviceToggle returns the text and link of the virtual-device control, and an
// empty label when there is nothing to say: a host with no virtual devices
// gets no control, because a page that offers to show nothing reads as a page
// that is hiding something.
//
// hidden is how many graphs were left out, so the link says what it will cost
// before you follow it. A host with sixty containers is a long page.
func deviceToggle(id int64, rangeKey string, showing bool, hidden int) (label, href string) {
	switch {
	case showing:
		return "hide virtual devices", hostHref(id, rangeKey, false)
	case hidden == 1:
		return "show 1 virtual device", hostHref(id, rangeKey, true)
	case hidden > 1:
		return fmt.Sprintf("show %d virtual devices", hidden), hostHref(id, rangeKey, true)
	}
	return "", ""
}
