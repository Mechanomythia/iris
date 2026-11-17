// Package sshagent is the SSH agent the sandbox talks to. It forwards to
// YOUR agent (1Password, Secretive, OpenSSH) only after the iris dialog, so
// Claude can sign git commits and log in over SSH with your keys without
// ever reaching the real agent socket or ~/.ssh.
//
// Allowed: list keys, sign (gated). Refused: add/remove keys, lock/unlock,
// extensions. Each sign request is classified so the popup can say what it
// is: an SSHSIG signature (git commit/tag signing, namespace "git") or an
// SSH user-authentication (login as <user>).
package sshagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Purpose of a sign request.
type Purpose struct {
	Kind   string // "signature" | "login" | "unknown"
	Detail string // SSHSIG namespace, or login user
}

// Target is the grant target: signature:git, login:git, sign:unknown.
func (p Purpose) Target() string {
	switch p.Kind {
	case "signature":
		return "signature:" + p.Detail
	case "login":
		return "login:" + p.Detail
	}
	return "sign:unknown"
}

// Classify inspects the data the agent is asked to sign.
func Classify(data []byte) Purpose {
	// SSHSIG (ssh-keygen -Y sign): "SSHSIG" + string namespace + ...
	if bytes.HasPrefix(data, []byte("SSHSIG")) {
		if ns, _, ok := readString(data[6:]); ok {
			return Purpose{Kind: "signature", Detail: sanitize(string(ns))}
		}
		return Purpose{Kind: "signature", Detail: "unknown"}
	}
	// User auth: string session_id, byte 50, string user, string service, string "publickey", ...
	if _, rest, ok := readString(data); ok && len(rest) > 0 && rest[0] == 50 {
		if user, rest2, ok := readString(rest[1:]); ok {
			if svc, rest3, ok := readString(rest2); ok && string(svc) == "ssh-connection" {
				if m, _, ok := readString(rest3); ok && string(m) == "publickey" {
					return Purpose{Kind: "login", Detail: sanitize(string(user))}
				}
			}
		}
	}
	return Purpose{Kind: "unknown"}
}

func readString(b []byte) ([]byte, []byte, bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

func sanitize(s string) string {
	var out strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@+", r) {
			out.WriteRune(r)
		}
		if out.Len() >= 64 {
			break
		}
	}
	if out.Len() == 0 {
		return "unknown"
	}
	return out.String()
}

// Gate asks the human. key is "<type> <comment>" for the popup.
type Gate func(ctx context.Context, p Purpose, key string, pid int, argv []string) error

// Upstream finds your real agent socket: configured path, then 1Password,
// then $SSH_AUTH_SOCK.
func Upstream(configured string) string {
	if configured != "" {
		return expand(configured)
	}
	h, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(h, "Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"),
		filepath.Join(h, ".1password/agent.sock"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return os.Getenv("SSH_AUTH_SOCK")
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// proxy implements agent.ExtendedAgent for one client connection.
type proxy struct {
	up   agent.ExtendedAgent
	gate Gate
	pid  int
	argv []string
}

var errRefused = errors.New("iris: this agent only lists keys and signs after approval")

func (p *proxy) List() ([]*agent.Key, error) { return p.up.List() }

func (p *proxy) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return p.SignWithFlags(key, data, 0)
}

func (p *proxy) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	label := key.Type()
	if keys, err := p.up.List(); err == nil {
		for _, k := range keys {
			if bytes.Equal(k.Blob, key.Marshal()) && k.Comment != "" {
				label = key.Type() + " " + k.Comment
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := p.gate(ctx, Classify(data), label, p.pid, p.argv); err != nil {
		return nil, err
	}
	return p.up.SignWithFlags(key, data, flags)
}

func (p *proxy) Add(agent.AddedKey) error                 { return errRefused }
func (p *proxy) Remove(ssh.PublicKey) error               { return errRefused }
func (p *proxy) RemoveAll() error                         { return errRefused }
func (p *proxy) Lock([]byte) error                        { return errRefused }
func (p *proxy) Unlock([]byte) error                      { return errRefused }
func (p *proxy) Signers() ([]ssh.Signer, error)           { return nil, errRefused }
func (p *proxy) Extension(string, []byte) ([]byte, error) { return nil, agent.ErrExtensionUnsupported }

// Server serves the iris agent socket.
type Server struct {
	Upstream func() string // your agent's socket, resolved per connection
	Gate     Gate
	// Peer returns the connecting process's PID and a description (argv of
	// it and its parent), for the popup.
	Peer func(net.Conn) (int, []string)
	Logf func(string, ...any)
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

func (s *Server) Serve(ctx context.Context, path string) error {
	if len(path) > 103 {
		return fmt.Errorf("ssh agent socket path too long: %s", path)
	}
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	upPath := ""
	if s.Upstream != nil {
		upPath = s.Upstream()
	}
	if upPath == "" {
		s.logf("ssh agent: no upstream agent (set ssh.upstream in the iris config)")
		return
	}
	up, err := net.DialTimeout("unix", upPath, 5*time.Second)
	if err != nil {
		s.logf("ssh agent: upstream %s: %v", upPath, err)
		return
	}
	defer up.Close()
	pid, argv := 0, []string(nil)
	if s.Peer != nil {
		pid, argv = s.Peer(c)
	}
	p := &proxy{up: agent.NewClient(up), gate: s.Gate, pid: pid, argv: argv}
	if err := agent.ServeAgent(p, c); err != nil && !errors.Is(err, io.EOF) {
		s.logf("ssh agent: %v", err)
	}
}
