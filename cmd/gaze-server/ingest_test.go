package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/alert"
	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/mailer"
)

func testServer(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token, err := s.Enroll(context.Background(), "web-01")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/reports", newIngest(s, testAlerter(s)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv, token
}

func postReports(t *testing.T, url, token string, body []byte, gzipped bool) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if gzipped {
		zw := gzip.NewWriter(&buf)
		zw.Write(body)
		zw.Close()
	} else {
		buf.Write(body)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/api/v1/reports", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestIngest covers the happy path the agent exercises: gzipped array in,
// rows stored, 204 out.
func TestIngest(t *testing.T) {
	s, srv, token := testServer(t)
	batch := []report.Report{{
		Schema:  report.Schema,
		Start:   time.Now().Add(-time.Minute),
		End:     time.Now(),
		Samples: 6,
	}}
	body, _ := json.Marshal(batch)

	resp := postReports(t, srv.URL, token, body, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	var rows int
	s.Read().QueryRow(`SELECT count(*) FROM reports`).Scan(&rows)
	if rows != 1 {
		t.Errorf("stored %d rows, want 1", rows)
	}
}

// TestIngestAuth covers the refusals: no token and a wrong token are both
// 401, and nothing is stored.
func TestIngestAuth(t *testing.T) {
	s, srv, _ := testServer(t)
	body, _ := json.Marshal([]report.Report{{}})

	for name, token := range map[string]string{"missing": "", "wrong": "bogus"} {
		if resp := postReports(t, srv.URL, token, body, false); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s token: status = %d, want 401", name, resp.StatusCode)
		}
	}
	var rows int
	s.Read().QueryRow(`SELECT count(*) FROM reports`).Scan(&rows)
	if rows != 0 {
		t.Errorf("unauthenticated posts stored %d rows", rows)
	}
}

// TestIngestRejectsNonArray pins the body shape.
func TestIngestRejectsNonArray(t *testing.T) {
	_, srv, token := testServer(t)
	if resp := postReports(t, srv.URL, token, []byte(`{"schema":1}`), false); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("object body: status = %d, want 400", resp.StatusCode)
	}
}

// TestIngestBusy pins the 429 path: with every slot taken, the reply says
// when to come back rather than queueing without bound.
func TestIngestBusy(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	h := newIngest(s, testAlerter(s))
	for range ingestSlots {
		h.slots <- struct{}{}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/reports", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After leaves the agent guessing")
	}
}

// testAlerter is a quiet alerter for tests whose subject is ingest, not
// alerting; alert behaviour has its own suite in internal/alert.
func testAlerter(s *store.Store) *alert.Alerter {
	return alert.New(s, &mailer.MemorySender{}, []string{"ops@example.com"})
}
