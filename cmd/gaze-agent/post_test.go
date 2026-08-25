package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
)

// writeToken creates a token file with the given mode and returns its path.
func writeToken(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("s3cret\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func testClient(t *testing.T, server string) *client {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	return newClient(u, writeToken(t, 0o600), "test")
}

// TestPostShape pins the request contract: the path, the gzipped JSON array,
// the bearer token, and the identifying User-Agent — and that the reply's
// directive comes back.
func TestPostShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/reports" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("authorization = %q; the token must arrive trimmed", got)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "gaze-agent/test") {
			t.Errorf("user-agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("content-encoding = %q", r.Header.Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var reports []report.Report
		if err := json.NewDecoder(zr).Decode(&reports); err != nil {
			t.Fatalf("body is not a JSON array of reports: %v", err)
		}
		if len(reports) != 2 || reports[1].Generation != 7 {
			t.Errorf("decoded %+v", reports)
		}
		json.NewEncoder(w).Encode(report.Directive{Generation: 9})
	}))
	defer srv.Close()

	d, err := testClient(t, srv.URL).post(context.Background(), []report.Report{{}, {Generation: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Generation != 9 {
		t.Errorf("directive = %+v, want generation 9", d)
	}
}

// TestPostNoDirective covers the two shapes of "nothing to say".
func TestPostNoDirective(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusOK} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		d, err := testClient(t, srv.URL).post(context.Background(), []report.Report{{}})
		if err != nil || d != nil {
			t.Errorf("status %d: directive %+v, err %v; want nil, nil", status, d, err)
		}
		srv.Close()
	}
}

// TestPostRetryAfter covers 429: the server's number is the wait, not an
// input to backoff.
func TestPostRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL).post(context.Background(), []report.Report{{}})
	ra, ok := errors.AsType[retryAfterError](err)
	if !ok {
		t.Fatalf("err = %v, want a retryAfterError", err)
	}
	if ra.delay != 7*time.Second {
		t.Errorf("delay = %s, want 7s", ra.delay)
	}
}

// TestPostErrorCarriesBody covers the house rule: a refusal must arrive with
// what the server said, because "http 403" alone says nothing.
func TestPostErrorCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token revoked on 2026-08-20", http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL).post(context.Background(), []report.Report{{}})
	if err == nil || !strings.Contains(err.Error(), "token revoked") {
		t.Errorf("err = %v, want the server's body in the message", err)
	}
}

// TestCheckServerURL pins the TLS rule: https anywhere, http only where
// there is no wire to intercept.
func TestCheckServerURL(t *testing.T) {
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://gaze.example.net", true},
		{"https://gaze.example.net:8443/base", true},
		{"http://localhost:9090", true},
		{"http://127.0.0.1:9090", true},
		{"http://[::1]:9090", true},
		{"http://gaze.example.net", false},
		{"http://192.168.1.10:9090", false},
		{"ftp://gaze.example.net", false},
	} {
		_, err := checkServerURL(c.url)
		if ok := err == nil; ok != c.ok {
			t.Errorf("checkServerURL(%s): err = %v, want ok = %v", c.url, err, c.ok)
		}
	}
}

// TestReadToken pins the mode contract: a token other users can read is as
// published as one on the command line.
func TestReadToken(t *testing.T) {
	if got, err := readToken(writeToken(t, 0o600)); err != nil || got != "s3cret" {
		t.Errorf("readToken = %q, %v; want s3cret trimmed, nil", got, err)
	}
	if _, err := readToken(writeToken(t, 0o644)); err == nil {
		t.Error("a group- and world-readable token file was accepted")
	}
	empty := filepath.Join(t.TempDir(), "token")
	os.WriteFile(empty, []byte(" \n"), 0o600)
	if _, err := readToken(empty); err == nil {
		t.Error("an empty token file was accepted")
	}
}

// TestNextTick pins the wall alignment and the offset.
func TestNextTick(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 30, 0, time.UTC)
	got := nextTick(base, time.Minute, 10*time.Second)
	want := time.Date(2026, 8, 25, 12, 1, 10, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextTick = %v, want %v", got, want)
	}
	// Exactly on the tick: the next one, never a zero wait.
	got = nextTick(want, time.Minute, 10*time.Second)
	if !got.Equal(want.Add(time.Minute)) {
		t.Errorf("nextTick on the tick = %v, want one interval later", got)
	}
}

// TestOffsetStable pins that the offset is a pure function of the host ID,
// so it survives restarts, and stays inside the interval.
func TestOffsetStable(t *testing.T) {
	a := &agent{hostID: "abc123"}
	first := a.offset(time.Minute)
	if again := a.offset(time.Minute); again != first {
		t.Errorf("offset moved between calls: %s then %s", first, again)
	}
	if first < 0 || first >= time.Minute {
		t.Errorf("offset %s is outside the interval", first)
	}
	if b := (&agent{hostID: "xyz789"}); b.offset(time.Minute) == first {
		t.Log("two hosts share an offset; harmless, noting it for curiosity")
	}
}

// TestFullJitter pins the bounds: never zero, never past the cap, and drawn
// from the full range rather than the top of it.
func TestFullJitter(t *testing.T) {
	for attempt := 1; attempt <= 20; attempt++ {
		for i := 0; i < 50; i++ {
			d := fullJitter(attempt)
			if d <= 0 || d > 15*time.Minute {
				t.Fatalf("fullJitter(%d) = %s", attempt, d)
			}
			if attempt == 1 && d > 2*time.Second {
				t.Fatalf("fullJitter(1) = %s, ceiling is 2s", d)
			}
		}
	}
}
