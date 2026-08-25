package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/report"
)

// staleAfter is how long after its last report a host is drawn as stale.
// Five minutes is several missed reports at the default interval, so a slow
// network or one lost POST does not flap the fleet list. Staleness reads
// receive time, so a host with a broken clock still goes stale.
const staleAfter = 5 * time.Minute

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

// fleetRow is one host on the list.
type fleetRow struct {
	query.Overview
	State      string
	StateClass string
	MemPct     float64
}

func (s *webServer) handleFleet(w http.ResponseWriter, r *http.Request) {
	fleet, err := s.q.Fleet(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows := make([]fleetRow, 0, len(fleet))
	for _, o := range fleet {
		row := fleetRow{Overview: o}
		row.State, row.StateClass = hostState(o.LastSeen)
		if o.MemTotal > 0 {
			row.MemPct = o.MemUsed / float64(o.MemTotal) * 100
		}
		rows = append(rows, row)
	}
	s.render(w, r, "fleet", page{Title: "Hosts", Authed: true, Data: rows})
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
	Active bool
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

	Graphs []graph // cpu, load, memory, swap
	Nets   []graph // one per interface
	Disks  []graph // one per device
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

	if key := r.URL.Query().Get("range"); key != "" {
		for _, rg := range ranges {
			if rg.Key == key {
				v.Range = key
			}
		}
	}
	var span time.Duration
	for _, rg := range ranges {
		v.Ranges = append(v.Ranges, rangeLink{Key: rg.Key, Active: rg.Key == v.Range})
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
	for _, n := range nets {
		v.Nets = append(v.Nets, buildGraph("net "+n.Name+" — rx / tx", from, to, 0, fmtYRate, false,
			series{class: "a", points: netPoints(n.Points, false)},
			series{class: "b", points: netPoints(n.Points, true)}))
	}

	disks, err := s.q.Disks(r.Context(), id, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, d := range disks {
		v.Disks = append(v.Disks, buildGraph("disk "+d.Name+" — read / write", from, to, 0, fmtYRate, false,
			series{class: "a", points: diskPoints(d.Points, false)},
			series{class: "b", points: diskPoints(d.Points, true)}))
	}

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
