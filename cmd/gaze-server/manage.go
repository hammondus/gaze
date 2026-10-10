package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
