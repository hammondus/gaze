package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/update"
)

// postBatch caps how many reports go in one POST, so a flushed backlog
// arrives in bounded pieces rather than one request the size of the outage.
const postBatch = 10

// client posts reports to the server.
type client struct {
	base      *url.URL
	tokenPath string
	version   string
	http      *http.Client
}

func newClient(base *url.URL, tokenPath, version string) *client {
	return &client{
		base:      base,
		tokenPath: tokenPath,
		version:   version,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// retryAfterError is a 429: the server said when to come back, and honouring
// that beats guessing with backoff.
type retryAfterError struct{ delay time.Duration }

func (e retryAfterError) Error() string {
	return fmt.Sprintf("server asked for a %s pause", e.delay)
}

// post sends a batch of reports, always as a JSON array, gzipped. It returns
// the directive from the reply, or nil when the server had nothing to say.
//
// The token is read per request rather than once at start-up, so rotating it
// on disk needs no restart.
func (c *client) post(ctx context.Context, reports []report.Report) (*report.Directive, error) {
	token, err := readToken(c.tokenPath)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if err := json.NewEncoder(zw).Encode(reports); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.JoinPath("api/v1/reports").String(), &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "gaze-agent/"+c.version+" (+"+update.Repo+")")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil, nil
	case resp.StatusCode == http.StatusOK:
		var d report.Directive
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil // an empty 200 is a reply with nothing to say
			}
			return nil, fmt.Errorf("undecodable directive: %w", err)
		}
		return &d, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, retryAfterError{delay: retryAfter(resp)}
	default:
		// The body usually says what happened; "http 403" alone does not.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("server said %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
}

// retryAfter reads the Retry-After header, in seconds or as an HTTP date,
// falling back to a minute when the server sent 429 without one.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return time.Minute
}

// checkServerURL refuses a server the agent should not talk to. The
// directive channel assumes TLS — everything DESIGN-DECISIONS says a spoofed
// server cannot do rests on it — so plain http is allowed only on loopback,
// where there is no wire to intercept.
func checkServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse -server: %w", err)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return u, nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return u, nil
		}
		return nil, fmt.Errorf("-server %s is plain http to a non-loopback host; use https", raw)
	default:
		return nil, fmt.Errorf("-server %s: the scheme must be https, or http on loopback", raw)
	}
}

// readToken reads the bearer token from its file, refusing a file other
// users can read: a token readable by group or other is as published as one
// passed on the command line, which is the thing a token file exists to
// avoid.
func readToken(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %s has mode %04o; it must be 0600", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// fullJitter is the recovery backoff: uniform over the full range up to an
// exponentially growing cap. The randomness is not a refinement — it is what
// decorrelates a herd of agents that all failed together, because plain
// exponential backoff has them all computing the same delay from the same
// outage.
func fullJitter(attempt int) time.Duration {
	const (
		base = time.Second
		cap  = 15 * time.Minute
	)
	ceiling := base << min(attempt, 62)
	if ceiling <= 0 || ceiling > cap {
		ceiling = cap
	}
	return time.Duration(rand.Int63n(int64(ceiling)) + 1)
}
