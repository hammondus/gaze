package main

import (
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"strings"

	"github.com/hammondus/mfa"
	"rsc.io/qr"
)

// The first-run setup flow. It exists only while no admin does: the server
// printed a one-time code to its log at start-up, and every step checks it,
// so a stranger who finds a freshly deployed server cannot claim the admin
// account, and cannot see the TOTP secret mid-enrolment either.
//
// The flow is one POST chain rather than stateful pages: account form →
// confirm page showing the QR code → confirmed. The QR image is embedded as
// a data: URI in the code-gated response, so the secret never has a URL of
// its own.

// setupData is what the setup templates show. QR is template.URL because
// html/template's URL filter rejects data: URIs; this one is built here
// from PNG bytes, not from input.
type setupData struct {
	Code     string // the setup code, carried through hidden fields
	Username string
	Secret   string // base32, for manual entry
	QR       template.URL
}

// setupOpen reports whether the setup flow is available, which requires
// both a code minted at start-up and no admin yet.
func (s *webServer) setupOpen(r *http.Request) bool {
	if s.setupCode == "" {
		return false
	}
	n, err := s.store.AdminCount(r.Context())
	return err == nil && n == 0
}

// codeOK checks the presented setup code in constant time.
func (s *webServer) codeOK(r *http.Request) bool {
	got := strings.TrimSpace(r.PostFormValue("setup_code"))
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.setupCode)) == 1
}

func (s *webServer) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if !s.setupOpen(r) {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "setup", page{Title: "Set up gaze"})
}

// handleSetup creates the admin, unconfirmed, and answers with the
// enrolment page. Submitting again restarts the enrolment with a fresh
// secret; an abandoned attempt is replaced, not resumed.
func (s *webServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.setupOpen(r) {
		http.NotFound(w, r)
		return
	}
	if !s.codeOK(r) {
		s.render(w, r, "setup", page{
			Title: "Set up gaze",
			Error: "That setup code is not right. It is in the server log.",
		})
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if username == "" || len(password) < 12 {
		s.render(w, r, "setup", page{
			Title: "Set up gaze",
			Error: "Enter a username and a password of at least 12 characters.",
		})
		return
	}

	secret, err := s.store.BeginSetup(r.Context(), s.key, username, password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderConfirm(w, r, username, secret, "")
}

// handleSetupConfirm verifies the first code and finishes the account.
func (s *webServer) handleSetupConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.setupOpen(r) {
		http.NotFound(w, r)
		return
	}
	if !s.codeOK(r) {
		http.NotFound(w, r)
		return
	}

	ok, err := s.store.ConfirmSetup(r.Context(), s.key,
		strings.TrimSpace(r.PostFormValue("code")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		username, secret, pending, err := s.store.PendingSetup(r.Context(), s.key)
		if err != nil || !pending {
			s.fail(w, r, err)
			return
		}
		s.renderConfirm(w, r, username, secret,
			"That code is not valid. Check the authenticator and try again.")
		return
	}

	// Done. The admin signs in through the front door like any other day.
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// renderConfirm draws the enrolment page: QR code, manual-entry secret, and
// the code form. It shows a secret, so it must not be stored anywhere.
func (s *webServer) renderConfirm(w http.ResponseWriter, r *http.Request, username, secret, errMsg string) {
	code, err := qr.Encode(mfa.URI("gaze", username, secret), qr.M)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store, private")
	s.render(w, r, "setup-confirm", page{
		Title: "Enrol the authenticator",
		Error: errMsg,
		Data: setupData{
			Code:     s.setupCode,
			Username: username,
			Secret:   secret,
			QR:       template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())),
		},
	})
}
