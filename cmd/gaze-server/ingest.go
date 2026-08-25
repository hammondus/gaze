package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/hammondus/gaze/internal/alert"
	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

const (
	// maxBody caps the raw request; maxDecoded caps what a gzip body may
	// inflate to, because the wire cap alone would wave a small bomb
	// through. A batch of ten reports is a few tens of kilobytes.
	maxBody    = 1 << 20
	maxDecoded = 8 << 20

	// ingestSlots bounds concurrent ingests. Ten hosts post once a minute;
	// a full house here means a recovering herd, and 429 with Retry-After
	// spreads it out — the server slows agents down rather than falling
	// over.
	ingestSlots = 8
)

// ingest is the one authenticated endpoint an agent talks to.
type ingest struct {
	store *store.Store
	alert *alert.Alerter
	dir   *directives
	slots chan struct{}
}

func newIngest(s *store.Store, a *alert.Alerter, d *directives) *ingest {
	return &ingest{store: s, alert: a, dir: d, slots: make(chan struct{}, ingestSlots)}
}

func (h *ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "30")
		http.Error(w, "busy; come back in 30 seconds", http.StatusTooManyRequests)
		return
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	hostID, err := h.store.Authenticate(r.Context(), token)
	if errors.Is(err, store.ErrUnknownToken) {
		http.Error(w, "unknown token: not enrolled, or revoked", http.StatusUnauthorized)
		return
	}
	if err != nil {
		log.Printf("ingest auth: %v", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}

	body := io.Reader(http.MaxBytesReader(w, r.Body, maxBody))
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "undecodable gzip body", http.StatusBadRequest)
			return
		}
		defer zr.Close()
		// LimitReader ends the stream at the cap, which surfaces as a
		// truncated-JSON refusal below rather than an unbounded inflate.
		body = io.LimitReader(zr, maxDecoded)
	}

	var batch []report.Report
	if err := json.NewDecoder(body).Decode(&batch); err != nil {
		http.Error(w, "body must be a JSON array of reports: "+err.Error(), http.StatusBadRequest)
		return
	}

	stored, err := h.store.InsertReports(r.Context(), hostID, batch)
	if err != nil {
		log.Printf("ingest host %d: %v", hostID, err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}
	if stored < len(batch) {
		log.Printf("ingest host %d: stored %d of %d reports (rest outside the accepted time range)", hostID, stored, len(batch))
	}

	// Threshold rules run here, while the report is already in memory. Only
	// the newest report of a batch is evaluated: the rest is backlog, and
	// the evaluator ignores anything past its live window anyway. A failure
	// must not fail the ingest — the report is stored, which is the part
	// the agent needs acknowledged.
	if stored > 0 {
		newest := &batch[0]
		for i := range batch {
			if batch[i].End.After(newest.End) {
				newest = &batch[i]
			}
		}
		if err := h.alert.Evaluate(r.Context(), hostID, newest); err != nil {
			log.Printf("alert host %d: %v", hostID, err)
		}
	}

	// Anything the server wants changed rides on this reply; it never
	// opens a connection to an agent. Nothing to say is a 204, and a
	// directive failure must not fail the ingest either — the report is
	// stored, which is the part the agent needs acknowledged.
	dir, err := h.dir.For(r.Context(), hostID)
	if err != nil {
		log.Printf("directive host %d: %v", hostID, err)
	}
	if dir == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(dir); err != nil {
		log.Printf("directive host %d: %v", hostID, err)
	}
}
