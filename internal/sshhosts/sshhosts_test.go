package sshhosts

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func line(t *testing.T, hosts ...string) string {
	pub, _, _ := ed25519.GenerateKey(nil)
	k, _ := ssh.NewPublicKey(pub)
	return knownhosts.Line(hosts, k) + "\n"
}

func TestEnsure(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "mine"), filepath.Join(d, "claude", "known_hosts")
	os.WriteFile(src, []byte(line(t, "github.com")+

		"garbage line ssh ignores\n"+
		line(t, knownhosts.HashHostname("gitlab.com"))+
		line(t, "[git.example.org]:2222")), 0o600)

	if s, _ := Ensure(src, dst, "gitlab.com", "22"); s != Synced {
		t.Fatalf("hashed gitlab.com: %s", s)
	}
	if s, _ := Ensure(src, dst, "gitlab.com", "22"); s != Known {
		t.Fatalf("second time: %s", s)
	}
	if s, _ := Ensure(src, dst, "git.example.org", "2222"); s != Known {
		t.Fatalf("non-default port: %s", s)
	}
	if s, _ := Ensure(src, dst, "git.example.org", "22"); s != Unknown {
		t.Fatalf("wrong port must not count: %s", s)
	}
	if s, _ := Ensure(src, dst, "evil.example", "22"); s != Unknown {
		t.Fatalf("unknown host: %s", s)
	}
}
