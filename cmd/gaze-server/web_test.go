package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
	"github.com/hammondus/mfa"
)

var testKey = make([]byte, mfa.KeySize)

// hotp reimplements RFC 4226 section 5.3, because the tests have to play
// the authenticator app; the mfa package correctly only verifies.
func hotp(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// totp returns the code `ahead` steps from now. Stepping forward is how a
// flow presents a second, unspent code without sleeping: VerifyTOTP
// accepts one step either side of now.
func totp(t *testing.T, secret string, ahead uint64) string {
	t.Helper()
	raw, err := mfa.DecodeSecret(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return hotp(raw, uint64(time.Now().Unix())/30+ahead)
}

// testWeb is a running web front end and a browser-shaped client against
// it: one cookie jar, redirects not followed so they can be asserted on.
type testWeb struct {
	t     *testing.T
	store *store.Store
	web   *webServer
	srv   *httptest.Server
	http  *http.Client
}

func newTestWeb(t *testing.T) *testWeb {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// Insecure cookies: httptest serves plain http, and the client's jar
	// honours the Secure attribute. The attribute itself is asserted
	// separately in TestCookieAttributes.
	web, err := newWebServer(s, testKey, false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	return &testWeb{
		t:     t,
		store: s,
		web:   web,
		srv:   srv,
		http: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (w *testWeb) get(path string) (*http.Response, string) {
	w.t.Helper()
	resp, err := w.http.Get(w.srv.URL + path)
	if err != nil {
		w.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// csrf returns the CSRF token currently in the jar, fetching a page first
// if none has been issued yet.
func (w *testWeb) csrf() string {
	w.t.Helper()
	u, _ := url.Parse(w.srv.URL)
	for range 2 {
		for _, c := range w.http.Jar.Cookies(u) {
			if c.Name == csrfCookie {
				return c.Value
			}
		}
		w.get("/login")
	}
	w.t.Fatal("no CSRF cookie issued")
	return ""
}

func (w *testWeb) post(path string, form url.Values) (*http.Response, string) {
	w.t.Helper()
	form.Set("csrf", w.csrf())
	resp, err := w.http.PostForm(w.srv.URL+path, form)
	if err != nil {
		w.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func wantRedirect(t *testing.T, resp *http.Response, to string) {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != to {
		t.Fatalf("redirects to %q, want %q", got, to)
	}
}

var secretRe = regexp.MustCompile(`class="secret">([A-Z2-7]+)<`)

// setupAndSignIn drives the whole first-run flow: setup code, account,
// authenticator enrolment, then the two-step login. It returns the TOTP
// secret for tests that need further codes.
func (w *testWeb) setupAndSignIn() string {
	w.t.Helper()

	_, body := w.post("/setup", url.Values{
		"setup_code": {w.web.setupCode},
		"username":   {"craig"},
		"password":   {"correct horse battery"},
	})
	m := secretRe.FindStringSubmatch(body)
	if m == nil {
		w.t.Fatalf("no secret on the enrolment page:\n%s", body)
	}
	secret := m[1]

	resp, _ := w.post("/setup/confirm", url.Values{
		"setup_code": {w.web.setupCode},
		"code":       {totp(w.t, secret, 0)},
	})
	wantRedirect(w.t, resp, "/login")

	resp, _ = w.post("/login", url.Values{
		"username": {"craig"},
		"password": {"correct horse battery"},
	})
	wantRedirect(w.t, resp, "/login/mfa")

	resp, _ = w.post("/login/mfa", url.Values{"code": {totp(w.t, secret, 1)}})
	wantRedirect(w.t, resp, "/")
	return secret
}

// TestNoPageWithoutASession is the stage's done-when, second half: every
// page that would render host data answers a signed-out request with a
// redirect and an empty hand.
func TestNoPageWithoutASession(t *testing.T) {
	w := newTestWeb(t)
	if _, err := w.store.Enroll(t.Context(), "secret-hostname"); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/hosts/1", "/hosts/enroll", "/nonsense"} {
		resp, body := w.get(path)
		wantRedirect(t, resp, "/login")
		if strings.Contains(body, "secret-hostname") {
			t.Fatalf("%s leaked host data to a signed-out request", path)
		}
	}

	// The POST half of enrolment is gated the same way.
	resp, _ := w.post("/hosts/enroll", url.Values{"name": {"evil"}})
	wantRedirect(t, resp, "/login")
}

func TestSetupThenSignIn(t *testing.T) {
	w := newTestWeb(t)
	w.setupAndSignIn()

	resp, body := w.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ after sign-in = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "No hosts are enrolled yet") {
		t.Fatalf("fleet page missing empty state:\n%s", body)
	}

	// Setup is over: the page is gone.
	if resp, _ := w.get("/setup"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/setup after setup = %d, want 404", resp.StatusCode)
	}
}

func TestSetupNeedsTheCode(t *testing.T) {
	w := newTestWeb(t)

	_, body := w.post("/setup", url.Values{
		"setup_code": {"wrong"},
		"username":   {"mallory"},
		"password":   {"long enough password"},
	})
	if secretRe.MatchString(body) {
		t.Fatal("a wrong setup code reached the enrolment page")
	}
	if n, _ := w.store.AdminCount(t.Context()); n != 0 {
		t.Fatal("a wrong setup code created an account")
	}
}

func TestWrongTOTPStaysOut(t *testing.T) {
	w := newTestWeb(t)

	_, body := w.post("/setup", url.Values{
		"setup_code": {w.web.setupCode},
		"username":   {"craig"},
		"password":   {"correct horse battery"},
	})
	secret := secretRe.FindStringSubmatch(body)[1]
	resp, _ := w.post("/setup/confirm", url.Values{
		"setup_code": {w.web.setupCode},
		"code":       {totp(w.t, secret, 0)},
	})
	wantRedirect(t, resp, "/login")

	resp, _ = w.post("/login", url.Values{
		"username": {"craig"},
		"password": {"correct horse battery"},
	})
	wantRedirect(t, resp, "/login/mfa")

	// The password cleared but the code has not: still no host data.
	resp, _ = w.get("/")
	wantRedirect(t, resp, "/login/mfa")

	if resp, _ := w.post("/login/mfa", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong code = %d, want the form again", resp.StatusCode)
	}
	resp, _ = w.get("/")
	wantRedirect(t, resp, "/login/mfa")
}

func TestCSRFRequired(t *testing.T) {
	w := newTestWeb(t)

	// A POST whose form lacks the token is refused before any handler.
	resp, err := w.http.PostForm(w.srv.URL+"/login", url.Values{
		"username": {"craig"}, "password": {"x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", resp.StatusCode)
	}
}

// TestStoredXSS enrols a report whose host-chosen strings are scripts, and
// asserts the pages render them inert. Anyone who can name a container on
// a monitored host writes into this page; see "Host-reported strings are
// untrusted input" in DESIGN-DECISIONS.md.
func TestStoredXSS(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()

	const payload = `<img src=x onerror=alert(1)>`
	token, err := w.store.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := w.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Report{
		Schema: report.Schema,
		Host:   report.Host{Hostname: "web-01", Kernel: "6.8.0", CPUCount: 4},
		Start:  time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		Containers:       []report.Container{{Name: payload, Image: payload, State: "running"}},
		Top:              []report.Process{{PID: 1, Name: payload, User: payload, Cmdline: payload}},
		Mounts:           []report.Mount{{Device: payload, Path: payload, FSType: "ext4", Total: 10, Used: 5, Percent: 50}},
		ContainerRuntime: "docker",
	}
	if _, err := w.store.InsertReports(ctx, hostID, []report.Report{r}); err != nil {
		t.Fatal(err)
	}

	w.setupAndSignIn()
	_, body := w.get("/hosts/1")
	if strings.Contains(body, payload) {
		t.Fatal("a host-reported string reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;img") {
		t.Fatal("the container name is missing entirely, not escaped")
	}
}

// TestFleetStates covers the done-when's first half as far as a test can:
// a never-reported host and a reporting one carry different state labels
// and the silent one shows dashes, not zeros. (Stale is the same rendering
// driven by hostState, covered in TestHostState.)
func TestFleetStates(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()

	if _, err := w.store.Enroll(ctx, "silent-01"); err != nil {
		t.Fatal(err)
	}
	token, err := w.store.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := w.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Report{
		Schema: report.Schema, Version: "v1.2.3",
		Host:  report.Host{Hostname: "web-01", CPUCount: 4},
		Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		CPU: report.Stat{Min: 10, Max: 50, Mean: 25},
		Memory: report.Gauge{Total: 8 << 30,
			Used: report.Stat{Min: 1 << 30, Max: 2 << 30, Mean: 3 << 29}},
		Procs: report.ProcCounts{Total: 100},
	}
	if _, err := w.store.InsertReports(ctx, hostID, []report.Report{r}); err != nil {
		t.Fatal(err)
	}

	w.setupAndSignIn()
	_, body := w.get("/")
	if !strings.Contains(body, "never reported") {
		t.Fatal("silent host is not labelled never reported")
	}
	if !strings.Contains(body, ">reporting<") {
		t.Fatal("live host is not labelled reporting")
	}
	if !strings.Contains(body, "—") {
		t.Fatal("silent host's figures are not dashes")
	}
}

func TestHostState(t *testing.T) {
	if label, _ := hostState(time.Time{}); label != "never reported" {
		t.Errorf("zero time = %q", label)
	}
	if label, _ := hostState(time.Now().Add(-time.Minute)); label != "reporting" {
		t.Errorf("1m ago = %q", label)
	}
	if label, _ := hostState(time.Now().Add(-time.Hour)); label != "stale" {
		t.Errorf("1h ago = %q", label)
	}
}

func TestEnrolPage(t *testing.T) {
	w := newTestWeb(t)
	w.setupAndSignIn()

	resp, body := w.post("/hosts/enroll", url.Values{"name": {"new-host"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enrol = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("token page Cache-Control = %q, want no-store", cc)
	}
	m := regexp.MustCompile(`class="token">([A-Za-z0-9_-]{43})<`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token on the page:\n%s", body)
	}

	// The token works against the store, which proves the page wraps the
	// same enrolment the CLI performs.
	if _, err := w.store.Authenticate(t.Context(), m[1]); err != nil {
		t.Fatalf("minted token does not authenticate: %v", err)
	}
}

func TestCacheAndSecurityHeaders(t *testing.T) {
	w := newTestWeb(t)

	resp, _ := w.get("/login")
	h := resp.Header
	if got := h.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("login Cache-Control = %q, want no-cache", got)
	}
	if h.Get("Content-Security-Policy") == "" {
		t.Error("no CSP header")
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if h.Get("ETag") == "" {
		t.Error("no ETag beside no-cache; revalidation has nothing to match")
	}

	w.setupAndSignIn()
	resp, _ = w.get("/")
	if got := resp.Header.Get("Cache-Control"); got != "no-cache, private" {
		t.Errorf("authed page Cache-Control = %q, want no-cache, private", got)
	}

	resp, _ = w.get("/static/style.css?v=" + w.web.assetVer)
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", got)
	}
	resp, _ = w.get("/static/style.css")
	if got := resp.Header.Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("unhashed asset Cache-Control = %q, must not be immutable", got)
	}
}

func TestCookieAttributes(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "gaze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	web, err := newWebServer(s, testKey, true) // secure, as deployed
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	web.setSessionCookie(rec, "tok")
	c := rec.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = Secure:%v HttpOnly:%v SameSite:%v", c.Secure, c.HttpOnly, c.SameSite)
	}
}

func TestLogout(t *testing.T) {
	w := newTestWeb(t)
	w.setupAndSignIn()

	resp, _ := w.post("/logout", url.Values{})
	wantRedirect(t, resp, "/login")
	resp, _ = w.get("/")
	wantRedirect(t, resp, "/login")
}
