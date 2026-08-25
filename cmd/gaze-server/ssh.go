// The SSH front end: ssh to the collector and land in the same Bubble Tea
// interface gaze renders locally, sourced from stored reports.
//
// This is hand-rolled against golang.org/x/crypto/ssh rather than an app
// framework — see "The SSH TUI is hand-rolled against x/crypto/ssh, not an
// app framework" in DESIGN-DECISIONS.md. The parts a framework would have
// hidden are owned explicitly here: the persisted host key, pty-req and
// window-change handling, and the one-session-per-connection rule.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hammondus/gaze/internal/query"
	"github.com/muesli/termenv"
	"golang.org/x/crypto/ssh"
)

// sshServer serves TUI sessions over SSH. Authentication is public-key
// only, against the allow-list file: an SSH session authenticated by a key
// already trusted for root on the monitored hosts is a stronger credential
// than a browser session, so it never touches passwords or mfa.
type sshServer struct {
	q        *query.Q
	addr     string
	keysPath string // authorized_keys-format allow-list
	signer   ssh.Signer
}

func newSSHServer(q *query.Q, addr, hostKeyPath, keysPath string) (*sshServer, error) {
	signer, err := loadHostKey(hostKeyPath)
	if err != nil {
		return nil, err
	}
	// An empty allow-list refuses everyone. Say so at start-up rather than
	// letting the operator discover it one rejected handshake at a time.
	if keys, err := loadAuthorizedKeys(keysPath); err != nil {
		log.Printf("ssh: %v: every connection will be refused until it exists", err)
	} else if len(keys) == 0 {
		log.Printf("ssh: %s lists no keys: every connection will be refused", keysPath)
	}
	return &sshServer{q: q, addr: addr, keysPath: keysPath, signer: signer}, nil
}

// loadHostKey reads the server's host key, generating it on first run. A
// server that changes identity on restart trains every operator to accept
// host-key warnings, so the key persists beside the database.
func loadHostKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("host key %s: %w", path, err)
		}
		return signer, nil

	case errors.Is(err, os.ErrNotExist):
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, "gaze-server host key")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			return nil, fmt.Errorf("write host key: %w", err)
		}
		signer, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			return nil, err
		}
		log.Printf("ssh: generated host key %s (%s)", path, ssh.FingerprintSHA256(signer.PublicKey()))
		return signer, nil

	default:
		return nil, fmt.Errorf("read host key: %w", err)
	}
}

// loadAuthorizedKeys parses the allow-list, in the authorized_keys format
// every operator already writes. It is read per handshake, not at
// start-up, so adding or revoking a key needs no restart.
func loadAuthorizedKeys(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for len(b) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(b)
		if err != nil {
			// A malformed line must not quietly shrink the allow-list to
			// its prefix; refuse the whole file so the operator fixes it.
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		keys[string(key.Marshal())] = true
		b = rest
	}
	return keys, nil
}

// serve accepts connections until ctx ends. Auth happens in the handshake:
// an unlisted key never reaches a channel, let alone a shell.
func (s *sshServer) serve(ctx context.Context) error {
	// The TUI renders through the ui package's styles, which resolve their
	// colours against this process's own stdout — a pipe or a container
	// log, never a terminal, so detection would strip every colour. The
	// clients are ssh sessions from real terminals; assume the 256-colour
	// dark default the palette was designed on.
	lipgloss.SetColorProfile(termenv.ANSI256)
	lipgloss.SetHasDarkBackground(true)

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	log.Printf("ssh: listening on %s, allow-list %s", s.addr, s.keysPath)
	return s.serveListener(ctx, ln)
}

// serveListener runs the accept loop on a listener the caller opened,
// which is also what lets the tests pick a port.
func (s *sshServer) serveListener(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *sshServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConfig())
	if err != nil {
		return // failed auth or a port scan; nothing to log per-connection
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(reqs)

	// One TUI session per connection. A second session channel on the same
	// connection is refused rather than multiplexed: this is a monitor,
	// not a shell host.
	started := false
	for newChan := range chans {
		if newChan.ChannelType() != "session" || started {
			newChan.Reject(ssh.Prohibited, "gaze serves one TUI session per connection")
			continue
		}
		started = true
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ctx, ch, requests)
	}
}

// sshConfig builds the handshake configuration. The allow-list is read
// inside the callback, per attempt, so a key added or revoked on disk
// takes effect without a restart.
func (s *sshServer) sshConfig() *ssh.ServerConfig {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			keys, err := loadAuthorizedKeys(s.keysPath)
			if err != nil {
				return nil, fmt.Errorf("allow-list unreadable")
			}
			if !keys[string(key.Marshal())] {
				return nil, fmt.Errorf("key not in allow-list")
			}
			return nil, nil
		},
	}
	config.AddHostKey(s.signer)
	return config
}

// ptyReq is the pty-req payload, RFC 4254 section 6.2.
type ptyReq struct {
	Term         string
	Cols, Rows   uint32
	WidthPx      uint32
	HeightPx     uint32
	EncodedModes string
}

// winChange is the window-change payload, RFC 4254 section 6.7.
type winChange struct {
	Cols, Rows uint32
	WidthPx    uint32
	HeightPx   uint32
}

// handleSession owns one session channel: it answers the pty and shell
// requests, runs the TUI over the channel, and forwards resizes — the
// pieces an app framework would have hidden.
func (s *sshServer) handleSession(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var p *tea.Program
	width, height := 80, 24
	havePty := false

	done := make(chan struct{})
	startTUI := func() {
		p = tea.NewProgram(
			newSSHRoot(s.q),
			tea.WithInput(ch),
			tea.WithOutput(ch),
			tea.WithAltScreen(),
			tea.WithContext(sessCtx),
			// The process's signals belong to the server, not to one
			// session's program.
			tea.WithoutSignalHandler(),
			tea.WithoutSignals(),
		)
		go func() {
			defer close(done)
			p.Run()
			// 0 regardless: q is how a session ends, and an ssh client
			// that reports failure for a normal quit cries wolf.
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		}()
		// The channel is not a tty the program can measure, so the size
		// arrives the way everything else does: as a message, from the
		// dimensions the pty-req carried.
		go p.Send(tea.WindowSizeMsg{Width: width, Height: height})
	}

	for {
		select {
		case req, ok := <-requests:
			if !ok {
				return
			}
			switch req.Type {
			case "pty-req":
				var pty ptyReq
				if err := ssh.Unmarshal(req.Payload, &pty); err == nil && pty.Cols > 0 {
					width, height = int(pty.Cols), int(pty.Rows)
				}
				havePty = true
				req.Reply(true, nil)

			case "window-change":
				var wc winChange
				if err := ssh.Unmarshal(req.Payload, &wc); err == nil && wc.Cols > 0 {
					width, height = int(wc.Cols), int(wc.Rows)
					if p != nil {
						go p.Send(tea.WindowSizeMsg{Width: width, Height: height})
					}
				}
				if req.WantReply {
					req.Reply(true, nil)
				}

			case "shell":
				if !havePty || p != nil {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				startTUI()

			case "env":
				req.Reply(true, nil) // accepted and ignored

			default:
				// exec, sftp, forwarding: this is a monitor, not a shell.
				if req.WantReply {
					req.Reply(false, nil)
				}
			}

		case <-done:
			return
		case <-sessCtx.Done():
			return
		}
	}
}
