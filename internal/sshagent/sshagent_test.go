package sshagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func str(s string) []byte {
	b := make([]byte, 4+len(s))
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	copy(b[4:], s)
	return b
}

func TestClassify(t *testing.T) {
	sig := append([]byte("SSHSIG"), str("git")...)
	if p := Classify(sig); p.Target() != "signature:git" {
		t.Fatal(p)
	}
	auth := append(str("session-id-bytes"), 50)
	auth = append(auth, str("git")...)
	auth = append(auth, str("ssh-connection")...)
	auth = append(auth, str("publickey")...)
	if p := Classify(auth); p.Target() != "login:git" {
		t.Fatal(p)
	}
	if p := Classify([]byte("random")); p.Target() != "sign:unknown" {
		t.Fatal(p)
	}
	// hostile namespace is sanitised
	if p := Classify(append([]byte("SSHSIG"), str("git\n\x1b[31mevil")...)); p.Detail != "git31mevil" {
		t.Fatal(p.Detail)
	}
}

func TestServerGatesAndRefuses(t *testing.T) {
	dir := t.TempDir()
	upSock, irisSock := filepath.Join(dir, "up.sock"), filepath.Join(dir, "iris.sock")
	kr := agent.NewKeyring()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	kr.Add(agent.AddedKey{PrivateKey: priv, Comment: "me@laptop"})
	ul, _ := net.Listen("unix", upSock)
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(kr, c)
		}
	}()
	allow := true
	var seen []string
	s := &Server{Upstream: func() string { return upSock },
		Gate: func(_ context.Context, p Purpose, key string, _ int, _ []string) error {
			seen = append(seen, p.Target()+" "+key)
			if !allow {
				return errors.New("denied by user")
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx, irisSock)
	time.Sleep(50 * time.Millisecond)

	c, err := net.Dial("unix", irisSock)
	if err != nil {
		t.Fatal(err)
	}
	cl := agent.NewClient(c)
	keys, err := cl.List()
	if err != nil || len(keys) != 1 || keys[0].Comment != "me@laptop" {
		t.Fatal(err, keys)
	}
	pub, _ := ssh.ParsePublicKey(keys[0].Blob)
	data := append([]byte("SSHSIG"), str("git")...)
	sig, err := cl.Sign(pub, data)
	if err != nil || pub.Verify(data, sig) != nil {
		t.Fatalf("approved sign: %v", err)
	}
	if len(seen) != 1 || seen[0] != "signature:git ssh-ed25519 me@laptop" {
		t.Fatal(seen)
	}
	allow = false
	if _, err := cl.Sign(pub, data); err == nil {
		t.Fatal("denied sign succeeded")
	}
	if cl.RemoveAll() == nil || cl.Lock([]byte("x")) == nil {
		t.Fatal("mutating ops must be refused")
	}
	if keys, _ := kr.List(); len(keys) != 1 {
		t.Fatal("upstream keyring was modified")
	}
}
