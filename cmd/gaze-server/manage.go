package main

import (
	"fmt"
	"net/http"
	"strconv"
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
	if err := s.store.RequestUpdateAll(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
