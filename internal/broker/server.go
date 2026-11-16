package broker

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mechanomythia/iris/internal/peer"
	"github.com/mechanomythia/iris/internal/protocol"
)

// Server owns the two sockets.
type Server struct {
	B *Broker
	// AllowClaudeControl disables the "control socket refuses Claude
	// descendants" check. Tests only.
	AllowClaudeControl bool
	Logf               func(format string, args ...any)
}

func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A stale socket from a previous run: only remove if nobody answers.
	if c, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		c.Close()
		return nil, errors.New(path + " is in use: is irisd already running?")
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Serve blocks until ctx is done.
func (s *Server) Serve(ctx context.Context, brokerSock, controlSock string) error {
	bl, err := listen(brokerSock)
	if err != nil {
		return err
	}
	cl, err := listen(controlSock)
	if err != nil {
		bl.Close()
		return err
	}
	go func() { <-ctx.Done(); bl.Close(); cl.Close() }()
	go s.acceptLoop(ctx, cl, s.handleControl)
	s.acceptLoop(ctx, bl, s.handleBroker)
	_ = os.Remove(brokerSock)
	_ = os.Remove(controlSock)
	return nil
}

func (s *Server) acceptLoop(ctx context.Context, l net.Listener, h func(context.Context, net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go h(ctx, c)
	}
}

// handleBroker serves the sandbox-facing socket. Ops that can only reduce
// access (revoke-session) or add advisory context are allowed; nothing here
// can approve.
func (s *Server) handleBroker(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	c := protocol.Wrap(nc)
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	var req protocol.Request
	if err := c.Recv(&req); err != nil {
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	switch req.Op {
	case "ping":
		_ = c.Send(protocol.Response{OK: true})
	case "credential":
		pid := peer.PID(nc)
		if pid == 0 {
			pid = req.PID
		}
		p := ProcInfo{PID: pid, Cwd: req.Cwd, Argv: req.Argv}
		// The helper is usually exec'd by kubectl/aws/git; show the parent's argv.
		if req.PPID > 0 {
			if inf, ok := peer.Lookup(req.PPID); ok && inf.Args != "" {
				p.Argv = []string{inf.Args}
			}
		}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { // client hung up (e.g. Ctrl-C) -> cancel the prompt
			_, _ = io.Copy(io.Discard, nc)
			cancel()
		}()
		cred, err := s.B.Credential(rctx, req.Tool, req.Target, p)
		if err != nil {
			_ = c.Send(protocol.Response{Error: err.Error()})
			return
		}
		_ = c.Send(protocol.Response{OK: true, Credential: cred})
	case "context":
		s.B.AddContext(req.Command, req.Cwd, req.SessionID)
		_ = c.Send(protocol.Response{OK: true})
	case "revoke-session":
		n := s.B.RevokeSession()
		_ = c.Send(protocol.Response{OK: true, Revoked: n})
	default:
		_ = c.Send(protocol.Response{Error: "unknown op on broker socket: " + req.Op})
	}
}

func (s *Server) handleControl(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	c := protocol.Wrap(nc)
	if !s.AllowClaudeControl {
		if pid := peer.PID(nc); pid > 0 && peer.IsClaudeDescendant(pid) {
			s.logf("refused control connection from Claude descendant pid %d", pid)
			_ = c.Send(protocol.Response{Error: "control socket refuses processes started by Claude Code"})
			return
		}
	}
	for {
		var req protocol.Request
		if err := c.Recv(&req); err != nil {
			return
		}
		switch req.Op {
		case "status":
			_ = c.Send(protocol.Response{OK: true, Status: s.B.Status()})
		case "grants":
			_ = c.Send(protocol.Response{OK: true, Grants: s.B.Grants.List()})
		case "revoke":
			n := 0
			if req.All {
				n = s.B.RevokeAll()
			} else {
				n = s.B.Revoke(req.ID)
			}
			_ = c.Send(protocol.Response{OK: true, Revoked: n})
		case "audit":
			n := req.N
			if n <= 0 {
				n = 50
			}
			_ = c.Send(protocol.Response{OK: true, Audit: s.B.Audit.Tail(n)})
		case "reload":
			if err := s.B.Reload(); err != nil {
				_ = c.Send(protocol.Response{Error: err.Error()})
			} else {
				_ = c.Send(protocol.Response{OK: true})
			}
		case "decision":
			if s.B.Hub.Decide(req.ID, req.Choice) {
				_ = c.Send(protocol.Response{OK: true})
			} else {
				_ = c.Send(protocol.Response{Error: "unknown or expired request"})
			}
		case "subscribe":
			s.subscribe(ctx, c)
			return
		default:
			_ = c.Send(protocol.Response{Error: "unknown op: " + req.Op})
		}
	}
}

// subscribe turns the connection into a duplex UI channel: events go out,
// decisions come in on the same connection.
func (s *Server) subscribe(ctx context.Context, c *protocol.Conn) {
	sub := s.B.Hub.Subscribe()
	defer s.B.Hub.Unsubscribe(sub)
	_ = c.Send(protocol.Response{OK: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var req protocol.Request
			if err := c.Recv(&req); err != nil {
				return
			}
			switch req.Op {
			case "decision":
				s.B.Hub.Decide(req.ID, req.Choice)
			case "revoke":
				if req.All {
					s.B.RevokeAll()
				} else {
					s.B.Revoke(req.ID)
				}
			}
		}
	}()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			if err := c.Send(ev); err != nil {
				return
			}
		case <-ping.C:
			if err := c.Send(protocol.Event{Type: "ping"}); err != nil {
				return
			}
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}
