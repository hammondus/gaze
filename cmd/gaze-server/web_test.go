package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
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
	path  string // the database file, for tests that steer stored times
	web   *webServer
	srv   *httptest.Server
	http  *http.Client
}

// testLatest is the latest release as the test server's gate sees it, and
// the test server's own version.
const testLatest = "v1.1.0"

func newTestWeb(t *testing.T) *testWeb {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gaze.db")
	s, err := store.Open(path)
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
	// A server on the latest release, which never reaches GitHub.
	web.gate = newUpdateGate(testLatest)
	web.gate.lookup = func() (string, error) { return testLatest, nil }
	srv := httptest.NewServer(web.handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	return &testWeb{
		t:     t,
		store: s,
		path:  path,
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

// TestHeaderVersion: a signed-in page names the server's build, and the
// sign-in page does not.
func TestHeaderVersion(t *testing.T) {
	w := newTestWeb(t)
	want := `<span class="version">` + testLatest + `</span>`

	if _, body := w.get("/login"); strings.Contains(body, `class="version"`) {
		t.Fatalf("sign-in page shows the version:\n%s", body)
	}
	w.setupAndSignIn()
	if _, body := w.get("/"); !strings.Contains(body, want) {
		t.Fatalf("host list header missing %s:\n%s", want, body)
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

// TestFleetCapacityAndPatching covers the columns a glance at the host list
// has to answer from: memory and the fullest disk coloured by the shared
// thresholds, a pending restart, and an update count that is counted, out
// of date, or never counted — three standings that must not look alike.
func TestFleetCapacityAndPatching(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()

	post := func(name string, memPct float64, mounts []report.Mount, reboot bool, u *report.Updates) {
		t.Helper()
		token, err := w.store.Enroll(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		id, err := w.store.Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		const total = 8 << 30
		used := memPct / 100 * total
		r := report.Report{
			Schema: report.Schema, Version: "v1.2.3",
			Host: report.Host{Hostname: name, CPUCount: 4, UptimeSeconds: 3*86400 + 4*3600,
				RebootRequired: reboot, Updates: u},
			Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
			Memory: report.Gauge{Total: total, Used: report.Stat{Min: used, Max: used, Mean: used}},
			Mounts: mounts,
			Procs:  report.ProcCounts{Total: 100},
		}
		if _, err := w.store.InsertReports(ctx, id, []report.Report{r}); err != nil {
			t.Fatal(err)
		}
	}
	post("full-01", 85, []report.Mount{
		{Path: "/", Device: "/dev/sda1", FSType: "ext4", Total: 100 << 30, Used: 50 << 30, Percent: 50},
		{Path: "/var", Device: "/dev/sda2", FSType: "ext4", Total: 20 << 30, Used: 19 << 30, Percent: 93},
	}, true, &report.Updates{Upgradable: 12, Security: 3, Counted: time.Now().Add(-time.Hour)})
	post("old-01", 50, nil, false, &report.Updates{Counted: time.Now().Add(-5 * 24 * time.Hour)})
	post("bare-01", 50, nil, false, nil)

	w.setupAndSignIn()
	_, body := w.get("/")
	if !strings.Contains(body, "as of "+time.Now().Format("2006-01-02")) {
		t.Error("host list does not say when it was read")
	}

	row := func(name string) string {
		t.Helper()
		i := strings.Index(body, ">"+name+"</a>")
		if i < 0 {
			t.Fatalf("no row for %s:\n%s", name, body)
		}
		end := strings.Index(body[i:], "</tr>")
		return body[i : i+end]
	}
	for _, c := range []struct{ host, want, why string }{
		{"full-01", `<span class="warn">85%</span>`, "memory over the warning point is not amber"},
		{"full-01", `<span class="bad">93%</span> <span class="dim mount">/var</span>`, "the fullest disk is not shown red"},
		{"full-01", `>restart</span>`, "a pending restart is not flagged"},
		{"full-01", `3d 4h`, "uptime is missing"},
		{"full-01", `12 · <span class="warn">3 security</span>`, "the update count is missing"},
		{"old-01", `<span class="">50%</span>`, "memory under the warning point is coloured"},
		{"old-01", `counted 5d ago`, "an out-of-date count does not say so"},
		{"old-01", `>none<`, "a zero count does not read as none"},
		{"bare-01", `No apt hook`, "an uncounted host looks counted"},
	} {
		if !strings.Contains(row(c.host), c.want) {
			t.Errorf("%s: %s; want %q in:\n%s", c.host, c.why, c.want, row(c.host))
		}
	}
	if strings.Contains(row("full-01"), "counted") {
		t.Error("a fresh count is marked as out of date")
	}
	if strings.Contains(row("bare-01"), "restart") {
		t.Error("restart flagged with no statement from the distribution")
	}

	// The host page carries the same standing, with the count's age always.
	_, body = w.get("/hosts/1")
	for _, want := range []string{"restart required", "updates pending: 12", "3 security", "(counted 1h 0m ago)"} {
		if !strings.Contains(body, want) {
			t.Errorf("host page missing %q", want)
		}
	}
}

// TestRefreshOnlyOnHostList pins the reload to the one page that is safe
// to reload: the enrolment result shows its token once, and a reload there
// would navigate away from it.
func TestRefreshOnlyOnHostList(t *testing.T) {
	w := newTestWeb(t)
	const meta = `http-equiv="refresh"`

	_, body := w.get("/login")
	if strings.Contains(body, meta) {
		t.Error("login page reloads itself")
	}

	w.setupAndSignIn()
	if _, body := w.get("/"); !strings.Contains(body, `<meta http-equiv="refresh" content="60; url=/">`) {
		t.Error("host list does not reload itself")
	}
	if _, body := w.get("/hosts/enroll"); strings.Contains(body, meta) {
		t.Error("enrol form reloads itself")
	}
	if _, body := w.post("/hosts/enroll", url.Values{"name": {"new-host"}}); strings.Contains(body, meta) {
		t.Error("token page reloads itself")
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

// TestAgentManagement is the stage-8 done-when through the browser: a
// change shows as sent, then as applied once the agent echoes it, and a
// declined directive is visible on the host list itself.
func TestAgentManagement(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()

	token, err := w.store.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := w.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	post := func(gen int, declined string) {
		t.Helper()
		r := report.Report{
			Schema: report.Schema, Version: "v1.0.0", Generation: gen, Declined: declined,
			Host:  report.Host{Hostname: "web-01"},
			Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		}
		if _, err := w.store.InsertReports(ctx, hostID, []report.Report{r}); err != nil {
			t.Fatal(err)
		}
	}
	post(0, "")
	w.setupAndSignIn()

	// Send a configuration; the host page says it is travelling.
	resp, _ := w.post("/hosts/1/config", url.Values{
		"sample_s": {"5"}, "report_s": {"30"}, "containers": {"leave"},
	})
	wantRedirect(t, resp, "/hosts/1")
	_, body := w.get("/hosts/1")
	if !strings.Contains(body, "gen 1 sent, agent at 0") {
		t.Fatalf("host page does not show the pending config:\n%s", body)
	}

	// The agent echoes it: applied, and visibly so.
	post(1, "")
	if _, body = w.get("/hosts/1"); !strings.Contains(body, "applied gen 1") {
		t.Fatal("host page does not show the applied config")
	}
	if _, body = w.get("/"); !strings.Contains(body, "applied gen 1") {
		t.Fatal("fleet list does not show the applied config")
	}

	// The agent declines instead: the refusal is on the host list, as
	// visible as an applied one, and the host page carries the why.
	post(1, "configuration generation 2 refused: started without -allow-remote-config")
	if _, body = w.get("/"); !strings.Contains(body, "declined") {
		t.Fatal("fleet list does not show the declined directive")
	}
	if _, body = w.get("/hosts/1"); !strings.Contains(body, "allow-remote-config") {
		t.Fatal("host page does not say why the agent declined")
	}

	// Nonsense intervals are refused at the door.
	resp, _ = w.post("/hosts/1/config", url.Values{"sample_s": {"0"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sample_s=0 accepted: %d", resp.StatusCode)
	}
	resp, _ = w.post("/hosts/1/config", url.Values{"sample_s": {"60"}, "report_s": {"30"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("report shorter than sample accepted: %d", resp.StatusCode)
	}

	// The update buttons record the request.
	resp, _ = w.post("/hosts/1/update", url.Values{})
	wantRedirect(t, resp, "/hosts/1")
	cfg, err := w.store.HostConfig(ctx, hostID)
	if err != nil || cfg.UpdateAsked.IsZero() {
		t.Fatalf("update request not recorded: %+v, %v", cfg, err)
	}
	if _, body = w.get("/hosts/1"); !strings.Contains(body, "update queued") {
		t.Fatal("host page does not show the pending update request")
	}
	resp, _ = w.post("/hosts/update-all", url.Values{})
	wantRedirect(t, resp, "/?asked=1")
}

// TestHostPageHidesVirtualDevices covers the web half of the device toggle. A
// container host stores a graph per veth, bridge, and loop device, which is
// the page's whole length spent on interfaces nobody asked about.
func TestHostPageHidesVirtualDevices(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()

	token, err := w.store.Enroll(ctx, "ctr-01")
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := w.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	busy := report.Stat{Min: 1, Max: 3, Mean: 2}
	r := report.Report{
		Schema: report.Schema,
		Host:   report.Host{Hostname: "ctr-01", CPUCount: 4},
		Start:  time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		Networks: []report.Network{
			{Name: "eth0", Rx: busy, Tx: busy, Up: true},
			{Name: "docker0", Rx: busy, Tx: busy, Up: true},
			{Name: "veth3a91c07", Rx: busy, Tx: busy, Up: true},
		},
		Disks: []report.Disk{
			{Name: "nvme0n1", Read: busy, Write: busy},
			{Name: "loop0", Read: busy},
		},
	}
	if _, err := w.store.InsertReports(ctx, hostID, []report.Report{r}); err != nil {
		t.Fatal(err)
	}

	w.setupAndSignIn()
	_, body := w.get("/hosts/1")
	for _, name := range []string{"docker0", "veth3a91c07", "loop0"} {
		if strings.Contains(body, name) {
			t.Errorf("%s is graphed on the default page", name)
		}
	}
	for _, name := range []string{"eth0", "nvme0n1"} {
		if !strings.Contains(body, name) {
			t.Errorf("%s is missing from the page", name)
		}
	}
	if !strings.Contains(body, "show 3 virtual devices") {
		t.Error("the page does not offer to show what it left out")
	}
	// The range links carry the device setting, and the device link the
	// range: one control must not reset the other.
	if !strings.Contains(body, `href="/hosts/1?range=1h"`) {
		t.Error("range links lost their href")
	}

	_, body = w.get("/hosts/1?virtual=1")
	for _, name := range []string{"eth0", "docker0", "veth3a91c07", "nvme0n1", "loop0"} {
		if !strings.Contains(body, name) {
			t.Errorf("%s is missing with ?virtual=1", name)
		}
	}
	if !strings.Contains(body, "hide virtual devices") {
		t.Error("no way back to the short page")
	}
	if !strings.Contains(body, `href="/hosts/1?range=1h&amp;virtual=1"`) {
		t.Error("a range link dropped the device setting")
	}

	// A host with nothing to hide gets no control at all.
	if _, err := w.store.Enroll(ctx, "plain-01"); err != nil {
		t.Fatal(err)
	}
	if _, body := w.get("/hosts/2"); strings.Contains(body, "virtual device") {
		t.Error("a host with no virtual devices still offers the toggle")
	}
}

// TestUpdateProgressOnHostList follows one update through the host list:
// the button's notice, queued, sent, not updated with the agent's reason,
// and done — and the held banner when the server itself is behind.
func TestUpdateProgressOnHostList(t *testing.T) {
	w := newTestWeb(t)
	ctx := t.Context()
	token, err := w.store.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	id, err := w.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	post := func(version, updateErr string) {
		t.Helper()
		r := report.Report{
			Schema: report.Schema, Version: version, UpdateError: updateErr,
			Host:  report.Host{Hostname: "web-01"},
			Start: time.Now().Add(-time.Minute), End: time.Now(), Samples: 6,
		}
		if _, err := w.store.InsertReports(ctx, id, []report.Report{r}); err != nil {
			t.Fatal(err)
		}
	}
	chip := func(body string) string {
		t.Helper()
		i := strings.Index(body, ">web-01</a>")
		if i < 0 {
			t.Fatalf("no row for web-01:\n%s", body)
		}
		row := body[i:]
		row = row[:strings.Index(row, "</tr>")]
		return row
	}
	post("v1.0.0", "")
	w.setupAndSignIn()

	// The button answers at once: a notice, and the row is queued.
	resp, _ := w.post("/hosts/update-all", url.Values{})
	wantRedirect(t, resp, "/?asked=1")
	_, body := w.get("/?asked=1")
	if !strings.Contains(body, "Asked 1 agent to update to "+testLatest) {
		t.Error("the update-all button leaves no notice")
	}
	if !strings.Contains(chip(body), ">queued<") {
		t.Errorf("row not queued:\n%s", chip(body))
	}
	if _, body = w.get("/"); strings.Contains(body, "Asked 1 agent") {
		t.Error("the notice outlives the reload")
	}

	// The server puts the trigger on a reply.
	if err := w.store.MarkUpdateSent(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, body = w.get("/"); !strings.Contains(chip(body), ">sent just now<") {
		t.Errorf("row not sent:\n%s", chip(body))
	}

	// Two minutes on, the agent reports again, still old, with its reason.
	db, err := sql.Open("sqlite", "file:"+w.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE hosts SET update_sent_at = update_sent_at - 120 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	post("v1.0.0", "cannot write to /usr/local/bin: read-only file system")
	_, body = w.get("/")
	if row := chip(body); !strings.Contains(row, ">not updated<") || !strings.Contains(row, "read-only file system") {
		t.Errorf("failure not shown with its reason:\n%s", row)
	}
	if _, body = w.get("/hosts/1"); !strings.Contains(body, "read-only file system") {
		t.Error("host page does not say why the update failed")
	}

	// It takes: only the new version shows.
	post(testLatest, "")
	_, body = w.get("/")
	if row := chip(body); strings.Contains(row, "chip pending") || strings.Contains(row, "chip bad") ||
		!strings.Contains(row, testLatest) {
		t.Errorf("done update still shows progress:\n%s", row)
	}

	// A server that is itself behind holds every request, and says so once.
	w.web.gate = newUpdateGate("dev")
	w.web.gate.lookup = func() (string, error) { return testLatest, nil }
	post("v1.0.0", "")
	if err := w.store.RequestUpdate(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, body = w.get("/")
	if !strings.Contains(body, "Agent updates are held: this server runs dev and the latest release is "+testLatest) {
		t.Errorf("no held banner:\n%s", body)
	}
	if !strings.Contains(chip(body), ">held<") {
		t.Errorf("row not held:\n%s", chip(body))
	}
}
