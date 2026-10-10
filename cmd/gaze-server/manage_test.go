package main

import (
	"strings"
	"testing"
	"time"
)

// TestUpdateStanding walks a request through every stage the host list
// shows. Each stage must read differently: a slow update and a stuck one
// are the two an operator most needs to tell apart.
func TestUpdateStanding(t *testing.T) {
	now := time.Now()
	asked := now.Add(-20 * time.Minute)
	sent := now.Add(-3 * time.Minute)
	base := updateFacts{Asked: asked, LastSeen: now, AgentVersion: "v0.5.0"}

	tests := []struct {
		name     string
		f        func(updateFacts) updateFacts
		latest   string
		server   string
		sendable bool
		label    string
		detail   string // a fragment the hover text must carry
	}{
		{name: "no request", f: func(f updateFacts) updateFacts { f.Asked = time.Time{}; return f },
			latest: "v0.6.1", sendable: true, label: ""},
		{name: "already current", f: func(f updateFacts) updateFacts { f.AgentVersion = "v0.6.1"; return f },
			latest: "v0.6.1", sendable: true, label: ""},
		{name: "refused", f: func(f updateFacts) updateFacts {
			f.Declined = "update refused: started without -allow-remote-update"
			return f
		}, latest: "v0.6.1", sendable: true, label: "refused", detail: "-allow-remote-update"},
		{name: "held, server behind", f: func(f updateFacts) updateFacts { return f },
			latest: "v0.6.1", server: "dev", sendable: false, label: "held", detail: "this server runs dev"},
		{name: "held, latest unknown", f: func(f updateFacts) updateFacts { return f },
			latest: "", server: "v0.6.1", sendable: false, label: "held", detail: "could not read the latest release"},
		{name: "queued, slot ahead", f: func(f updateFacts) updateFacts {
			f.Asked = now.Add(-time.Minute)
			f.Slot = 10 * time.Minute
			return f
		}, latest: "v0.6.1", sendable: true, label: "queued", detail: "first report after"},
		{name: "queued, slot passed", f: func(f updateFacts) updateFacts { return f },
			latest: "v0.6.1", sendable: true, label: "queued", detail: "next report"},
		{name: "sent, no report since", f: func(f updateFacts) updateFacts {
			f.Sent = sent
			f.LastSeen = sent.Add(2 * time.Second)
			return f
		}, latest: "v0.6.1", sendable: true, label: "sent 3m ago", detail: "waiting for the agent's next report"},
		{name: "not updated, reason known", f: func(f updateFacts) updateFacts {
			f.Sent = sent
			f.UpdateError = "cannot write to /usr/local/bin: read-only file system"
			return f
		}, latest: "v0.6.1", sendable: true, label: "not updated", detail: "read-only file system"},
		{name: "not updated, older agent", f: func(f updateFacts) updateFacts { f.Sent = sent; return f },
			latest: "v0.6.1", sendable: true, label: "not updated", detail: "journalctl -u gaze-agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateStanding(tt.f(base), tt.latest, tt.server, tt.sendable)
			if got.Label != tt.label {
				t.Errorf("label = %q, want %q (detail %q)", got.Label, tt.label, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.detail)
			}
		})
	}
}

func TestAskedNotice(t *testing.T) {
	for _, c := range []struct{ asked, latest, want string }{
		{"3", "v0.6.1", "Asked 3 agents to update to v0.6.1."},
		{"1", "v0.6.1", "Asked 1 agent to update to v0.6.1."},
		{"0", "v0.6.1", "already on v0.6.1"},
		{"junk", "v0.6.1", ""},
	} {
		got := askedNotice(c.asked, c.latest)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("askedNotice(%q) = %q, want %q", c.asked, got, c.want)
		}
	}
}
