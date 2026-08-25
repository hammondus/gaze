// The web front end: session-gated pages over the query package. The
// pattern here — double-submit CSRF, buffered rendering, the cache policy,
// the two-step login — is ported from mfademo, the worked example the mfa
// module ships with.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hammondus/gaze/internal/query"
	"github.com/hammondus/gaze/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	sessionCookie = "gaze_session"
	csrfCookie    = "gaze_csrf"

	// sessionTTL mirrors the row expiry in the store, so a stale cookie is
	// not sent for hours after it stopped working.
	sessionTTL = 12 * time.Hour
)

// webServer holds what the page handlers share. All host data flows through
// q; the store is written only for enrolment, sessions, and the admin
// account.
type webServer struct {
	store *store.Store
	q     *query.Q

	// key seals TOTP secrets, from GAZE_KEY at start-up — never a file
	// beside the database it would be protecting.
	key []byte

	// setupCode gates the first-run setup flow. Non-empty only when the
	// server started with no admin; it is printed to the log, and holding
	// it is what stops a stranger who finds a freshly deployed server from
	// claiming the admin account first.
	setupCode string

	// pages holds one template set per page: base.html plus that page's
	// file. Parsing them separately means every page can define a block
	// called "content" without the last one parsed winning.
	pages map[string]*template.Template

	// assetVer is a hash of the embedded static files. The files cannot
	// change without a rebuild, so hashing once at start-up is correct
	// here; read them from disk instead and this has to move to
	// per-request.
	assetVer string

	// secure marks cookies Secure. On by default: the deployed server sits
	// behind the TLS proxy the agents already require. -insecure-cookies
	// exists for plain-HTTP local development, where a Secure cookie is
	// simply dropped.
	secure bool
}

func newWebServer(s *store.Store, key []byte, secure bool) (*webServer, error) {
	w := &webServer{
		store:  s,
		q:      query.New(s.Read()),
		key:    key,
		secure: secure,
	}
	if err := w.parseTemplates(); err != nil {
		return nil, err
	}
	ver, err := hashAssets()
	if err != nil {
		return nil, err
	}
	w.assetVer = ver

	n, err := s.AdminCount(context.Background())
	if err != nil {
		return nil, err
	}
	if n == 0 {
		raw := make([]byte, 6)
		rand.Read(raw)
		w.setupCode = hex.EncodeToString(raw)
		log.Printf("no admin account exists. To create one, open /setup and enter the code %s", w.setupCode)
	}
	return w, nil
}

func (s *webServer) parseTemplates() error {
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, name := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(name, "templates/"), ".html")
		if base == "base" {
			continue
		}
		t, err := template.New("base.html").Funcs(templateFuncs).
			ParseFS(assets, "templates/base.html", name)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		s.pages[base] = t
	}
	if len(s.pages) == 0 {
		return errors.New("no page templates found")
	}
	return nil
}

func hashAssets() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(assets, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// handler returns the web front end as one http.Handler, for main to mount
// beside the ingest API. The shape is the security argument: the open mux
// lists exactly the pages that exist before a session — login, the second
// factor, first-run setup, static files — and everything else falls through
// to the authed mux behind requireAdmin. A page that renders host data
// without a session would have to be registered in the wrong mux to exist.
func (s *webServer) handler() http.Handler {
	authed := http.NewServeMux()
	authed.HandleFunc("GET /{$}", s.handleFleet)
	authed.HandleFunc("GET /hosts/{id}", s.handleHost)
	authed.HandleFunc("POST /hosts/{id}/config", s.handleHostConfig)
	authed.HandleFunc("POST /hosts/{id}/update", s.handleHostUpdate)
	authed.HandleFunc("POST /hosts/update-all", s.handleUpdateAll)
	authed.HandleFunc("GET /hosts/enroll", s.handleEnrollForm)
	authed.HandleFunc("POST /hosts/enroll", s.handleEnroll)
	authed.HandleFunc("POST /logout", s.handleLogout)

	open := http.NewServeMux()
	open.HandleFunc("GET /static/", s.handleStatic)
	open.HandleFunc("GET /login", s.handleLoginForm)
	open.HandleFunc("POST /login", s.handleLogin)
	open.HandleFunc("GET /login/mfa", s.handleMFAForm)
	open.HandleFunc("POST /login/mfa", s.handleMFA)
	open.HandleFunc("GET /setup", s.handleSetupForm)
	open.HandleFunc("POST /setup", s.handleSetup)
	open.HandleFunc("POST /setup/confirm", s.handleSetupConfirm)
	open.Handle("/", s.requireAdmin(authed))

	return s.withCSRF(open)
}

// requireAdmin resolves a fully authenticated session, redirecting anyone
// else to the step they are missing.
func (s *webServer) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		switch {
		case !ok:
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case !sess.Authed:
			http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// withCSRF issues the CSRF cookie and checks it on every unsafe request.
//
// This is the double-submit pattern: the token lives in a cookie and is
// echoed in a form field, and a cross-site page can send the cookie but
// cannot read it to fill in the field. One mechanism covers the login and
// setup forms as well, which a session-bound token cannot, because at that
// point there is no session to bind to. SameSite=Lax already blocks the
// cross-site POST this defends against; it is here anyway, because SameSite
// is a second line, not the only one.
func (s *webServer) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.csrfToken(w, r)

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(token)) != 1 {
				http.Error(w, "CSRF token mismatch. Reload the page and try again.",
					http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// csrfToken returns the request's CSRF token, minting one if the cookie is
// missing or malformed.
func (s *webServer) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == 43 {
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	// The cookie is not on the request that created it, so put it where the
	// same request's template can still find it.
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
	return token
}

// handleStatic serves the embedded assets, per the house caching policy: a
// request carrying the current ?v= names these exact bytes and can never go
// stale, so it gets a year and immutable. Anything else — a bookmarked
// path, a favicon — gets an hour, because that URL's content can change
// under a client.
func (s *webServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") == s.assetVer {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.FileServerFS(assets).ServeHTTP(w, r)
}

// page carries what every template can show, plus the page's own Data.
type page struct {
	Title  string
	Asset  string // cache-busting query for static assets
	CSRF   string
	Error  string
	Authed bool // whether to draw the signed-in chrome (nav, logout)
	Data   any
}

// render is the one place a page is written, so the cache policy is set
// once and the strict cases — no-store on the pages that show a secret —
// are opt-in and visible at their call sites.
func (s *webServer) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.fail(w, r, fmt.Errorf("no template %q", name))
		return
	}

	p.Asset = s.assetVer
	p.CSRF = s.csrfToken(w, r)

	// Buffer first. A template that fails halfway would otherwise have
	// already sent 200 and half a page, leaving no way to report the error.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", p); err != nil {
		s.fail(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		// HTML is never plainly cacheable: a stale page defeats the asset
		// cache-busting it carries. no-cache means store but always
		// revalidate; private keeps a session's page out of shared caches.
		if p.Authed {
			h.Set("Cache-Control", "no-cache, private")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")

	// Every page depends on the session cookie. Without this, a cache keys
	// only on the URL and may hand one session's page to another.
	h.Set("Vary", "Cookie")

	// no-cache means "always revalidate", and a browser can only
	// revalidate against a validator; without one Chrome serves the stored
	// copy instead. The page is buffered, so a strong ETag costs one hash.
	// no-store pages must not be stored at all, so they skip it.
	if !strings.Contains(h.Get("Cache-Control"), "no-store") {
		sum := sha256.Sum256(buf.Bytes())
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		h.Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatch(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Write(buf.Bytes())
}

// etagMatch reports whether an If-None-Match header lists etag. The header
// is a comma-separated list and may be "*".
func etagMatch(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (s *webServer) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("web: %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "Something went wrong. Check the server log.", http.StatusInternalServerError)
}

// setSessionCookie writes the session token.
func (s *webServer) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func (s *webServer) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// session returns the request's session, or false if there is none.
func (s *webServer) session(r *http.Request) (store.AdminSession, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.AdminSession{}, false
	}
	sess, ok, err := s.store.AdminSessionByToken(r.Context(), c.Value)
	if err != nil || !ok {
		return store.AdminSession{}, false
	}
	return sess, true
}
