// Package sshhosts keeps Claude's copy of known_hosts current. The sandbox
// can't read ~/.ssh, so Claude's ssh verifies hosts against a copy; a host
// you trusted after `iris install` would otherwise look like a blocked one.
package sshhosts

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Status of a host in Claude's copy after Ensure.
const (
	Known   = "known"   // already in Claude's copy
	Synced  = "synced"  // was missing; copied from your known_hosts
	Unknown = "unknown" // not in your known_hosts either: you have to trust it first
)

var probeKey ssh.PublicKey

func init() {
	pub, _, _ := ed25519.GenerateKey(nil)
	probeKey, _ = ssh.NewPublicKey(pub)
}

// Has reports whether file has any key for host:port (hashed entries too).
func Has(file, host, port string) bool {
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	// One line Go can't parse fails knownhosts.New for the whole file, while
	// ssh just skips it: check against the lines that parse.
	var ok []byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if _, _, _, _, _, err := ssh.ParseKnownHosts(l); err == nil {
			ok = append(append(ok, l...), '\n')
		}
	}
	tmp, err := os.CreateTemp("", "iris-known-hosts-*")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	_, _ = tmp.Write(ok)
	tmp.Close()
	cb, err := knownhosts.New(tmp.Name())
	if err != nil {
		return false
	}
	p, _ := strconv.Atoi(port)
	if p == 0 {
		p = 22
	}
	addr := &net.TCPAddr{IP: net.IPv4zero, Port: p}
	err = cb(net.JoinHostPort(host, strconv.Itoa(p)), addr, probeKey)
	var ke *knownhosts.KeyError
	// A random key never matches: a known host reports the keys it wants.
	return errors.As(err, &ke) && len(ke.Want) > 0
}

// Ensure makes sure Claude's copy (dst) knows host:port, re-copying your
// known_hosts (src) when it doesn't. Never trusts a key you haven't.
func Ensure(src, dst, host, port string) (string, error) {
	if Has(dst, host, port) {
		return Known, nil
	}
	if !Has(src, host, port) {
		return Unknown, nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return Unknown, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Unknown, err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return Unknown, err
	}
	return Synced, os.Rename(tmp, dst)
}
