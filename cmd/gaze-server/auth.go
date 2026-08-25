package main

import (
	"net/http"
	"strings"
)

// handleLoginForm shows the password form, or moves an existing session
// along to wherever it was headed.
func (s *webServer) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.session(r); ok {
		if sess.Authed {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
		}
		return
	}
	s.render(w, r, "login", page{Title: "Sign in"})
}

// handleLogin verifies the password and nothing else. The session it
// creates is unauthed: it exists, but reaches nothing except the code
// prompt until the second factor promotes it. Holding that state in the
// database rather than in the cookie means the client cannot promote
// itself. The response to a wrong password says nothing about whether the
// account exists or is locked.
func (s *webServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	id, ok, err := s.store.CheckAdminPassword(r.Context(), username, password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, "login", page{
			Title: "Sign in",
			Error: "Username or password is incorrect.",
		})
		return
	}

	token, err := s.store.NewAdminSession(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setSessionCookie(w, token)
	http.Redirect(w, r, "/login/mfa", http.StatusSeeOther)
}

// handleMFAForm shows the code prompt to a session that has cleared the
// password only.
func (s *webServer) handleMFAForm(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if sess.Authed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "mfa", page{Title: "Second factor"})
}

// handleMFA spends a TOTP code and promotes the session only if it is
// accepted. A locked account looks identical to a wrong code: telling
// someone their lockout has started tells them when to come back.
func (s *webServer) handleMFA(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if sess.Authed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	accepted, err := s.store.CheckAdminTOTP(r.Context(), s.key, sess.AdminID,
		strings.TrimSpace(r.PostFormValue("code")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !accepted {
		s.render(w, r, "mfa", page{
			Title: "Second factor",
			Error: "That code is not valid.",
		})
		return
	}

	if err := s.store.PromoteAdminSession(r.Context(), sess.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *webServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.session(r); ok {
		if err := s.store.DeleteAdminSession(r.Context(), sess.ID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
