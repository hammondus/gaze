package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

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
	slots chan struct{}
}

func newIngest(s *store.Store) *ingest {
	return &ingest{store: s, slots: make(chan struct{}, ingestSlots)}
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
	// No directive machinery yet: stage 8 grows this reply.
	w.WriteHeader(http.StatusNoContent)
}
