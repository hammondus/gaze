package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hammondus/gaze/internal/store"
)

// Agent management handlers: the desired configuration and the update
// trigger. Both only record intent — everything reaches the agent on its
// next report's reply, and the agent's flags decide whether it complies.

// parseSeconds reads an optional seconds field: blank means zero, which
// means "leave the agent's own value alone".
func parseSeconds(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 3600 {
		return 0, fmt.Errorf("%q is not a number of seconds between 1 and 3600", v)
	}
	return n, nil
}

func (s *webServer) handleHostConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	sample, err := parseSeconds(r.PostFormValue("sample_s"))
	if err != nil {
		http.Error(w, "sample interval: "+err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := parseSeconds(r.PostFormValue("report_s"))
	if err != nil {
		http.Error(w, "report interval: "+err.Error(), http.StatusBadRequest)
		return
	}
	if sample > 0 && rep > 0 && rep < sample {
		http.Error(w, "the report interval must be at least the sample interval", http.StatusBadRequest)
		return
	}

	var containers *bool
	switch r.PostFormValue("containers") {
	case "", "leave":
	case "on":
		v := true
		containers = &v
	case "off":
		v := false
		containers = &v
	default:
		http.Error(w, "containers must be leave, on, or off", http.StatusBadRequest)
		return
	}

	if _, err := s.store.SetHostConfig(r.Context(), id, sample, rep, containers); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/hosts/%d", id), http.StatusSeeOther)
}

func (s *webServer) handleHostUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RequestUpdate(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/hosts/%d", id), http.StatusSeeOther)
}

func (s *webServer) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	// The latest release decides who needs asking. The lookup is the
	// gate's own hourly-cached one, so a click costs GitHub nothing extra.
	latest, _ := s.gate.check()
	n, err := s.store.RequestUpdateAll(r.Context(), latest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/?asked="+strconv.Itoa(n), http.StatusSeeOther)
}

// updateFacts is what the server knows about one host's update request.
type updateFacts struct {
	Asked, Sent  time.Time // zero: no request; not sent yet
	LastSeen     time.Time // the host's latest report, server clock
	AgentVersion string
	Declined     string // the agent's refusal text, if any
	UpdateError  string // the agent's text for its last failed attempt
	Slot         time.Duration
}

// updateView is where a host's update stands, for the host list and the
// host page. An empty Label means nothing to show: no request, or the agent
// is already on the latest release.
type updateView struct {
	Label  string // queued, held, sent …, not updated, refused
	Class  string // chip class
	Detail string // one sentence: hover text on the list, prose on the host page
}

// reportGrace is how long after the trigger a report must arrive before
// its old version counts as "not updated". The agent installs and re-execs
// within seconds, and the new process reports a full interval later; a
// report inside the grace can come from the old process mid-download.
const reportGrace = 30 * time.Second

// updateStanding walks a request through its stages: queued until the
// host's slot and next report, held while this server is not on the latest
// release, sent once a reply carried the trigger, and then either done —
// the new version shows and the request clears — or not updated, which a
// report after the send still carrying the old version proves.
//
// "Not updated" reads the agent's own reason when it sent one. Agents older
// than the field send none, and the page says where to look instead.
func updateStanding(f updateFacts, latest, serverVersion string, sendable bool) updateView {
	switch {
	case f.Asked.IsZero():
		return updateView{}
	case latest != "" && f.AgentVersion == latest:
		return updateView{} // done; the next report clears the request
	case strings.Contains(f.Declined, "-allow-remote-update"):
		return updateView{"refused", "bad",
			"The agent refused: it runs without -allow-remote-update. Add the flag to ExecStart on the host."}
	case !sendable && latest == "":
		return updateView{"held", "warn",
			"Held: the server could not read the latest release from GitHub. It tries again hourly; make logs has the error."}
	case !sendable:
		return updateView{"held", "warn",
			fmt.Sprintf("Held: this server runs %s and the latest release is %s. Deploy %s with make deploy.",
				serverVersion, latest, latest)}
	case f.Sent.IsZero():
		if due := f.Asked.Add(f.Slot); time.Now().Before(due) {
			return updateView{"queued", "pending",
				"Queued: sends with this host's first report after " + due.Format("15:04") + "."}
		}
		return updateView{"queued", "pending", "Queued: sends with this host's next report."}
	case f.LastSeen.Before(f.Sent.Add(reportGrace)):
		return updateView{"sent " + fmtAgo(f.Sent), "pending",
			"Sent at " + f.Sent.Format("15:04:05") + "; waiting for the agent's next report."}
	case f.UpdateError != "":
		return updateView{"not updated", "bad",
			"Not updated: " + f.UpdateError + ". The agent retries hourly while the request stands."}
	default:
		return updateView{"not updated", "bad",
			fmt.Sprintf("Not updated: the agent reported again on %s after the update was sent at %s. "+
				"This agent does not report why; journalctl -u gaze-agent on the host does.",
				f.AgentVersion, f.Sent.Format("15:04:05"))}
	}
}

// labelMax bounds a label in runes. A label is a short name for a column
// or a graph caption, and a long one would push the host list back into
// the wrapping the labels exist to avoid.
const labelMax = 32

// parseLabel trims and checks one submitted label. Empty is valid and
// means remove.
func parseLabel(v string) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) {
		return "", errors.New("label is not valid UTF-8")
	}
	if utf8.RuneCountInString(v) > labelMax {
		return "", fmt.Errorf("label is longer than %d characters", labelMax)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errors.New("label contains a control character")
		}
	}
	return v, nil
}

// handleHostLabels stores the labels form: one field per mount, interface,
// and block device, named label.<kind>.<reported name>. Every field on the
// form is written, blank ones as a removal, so clearing a box clears the
// label. Names not on the form are left alone.
func (s *webServer) handleHostLabels(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Check the whole form before writing any of it, so one bad box does
	// not leave the others half saved.
	type entry struct{ kind, name, label string }
	var entries []entry
	for field, values := range r.PostForm {
		rest, ok := strings.CutPrefix(field, "label.")
		if !ok {
			continue
		}
		// The kind never contains a dot; the name, a mount path, may.
		kind, name, ok := strings.Cut(rest, ".")
		if !ok || name == "" {
			http.Error(w, "malformed label field "+field, http.StatusBadRequest)
			return
		}
		switch kind {
		case store.LabelMount, store.LabelNet, store.LabelDisk:
		default:
			http.Error(w, "unknown label kind "+kind, http.StatusBadRequest)
			return
		}
		label, err := parseLabel(values[0])
		if err != nil {
			http.Error(w, name+": "+err.Error(), http.StatusBadRequest)
			return
		}
		entries = append(entries, entry{kind, name, label})
	}
	for _, e := range entries {
		if err := s.store.SetLabel(r.Context(), id, e.kind, e.name, e.label); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/hosts/%d", id), http.StatusSeeOther)
}

// configStatus is the one line that answers "did it take?": applied,
// still travelling, or refused — three different facts, and the last one
// needs a person to walk over and change a flag.
func configStatus(echoed, cfgGen int, declined string) (label, class string) {
	switch {
	case declined != "":
		return "declined", "bad"
	case cfgGen == 0:
		return "", ""
	case echoed == cfgGen:
		return fmt.Sprintf("applied gen %d", cfgGen), "ok"
	default:
		return fmt.Sprintf("gen %d sent, agent at %d", cfgGen, echoed), "pending"
	}
}
