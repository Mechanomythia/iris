package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/broker"
	"github.com/mechanomythia/iris/internal/config"
)

// echo server standing in for gitlab.com:22
func echoServer(t *testing.T) string {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func gate(t *testing.T) (*Proxy, *atomic.Value, *atomic.Int32, *[]string) {
	t.Helper()
	echo := echoServer(t)
	cfg := config.Default()
	cfg.Proxy.Enabled = true
	cfg.Proxy.UnknownHosts = "ask"
	b := broker.New(cfg, "", audit.Open(filepath.Join(t.TempDir(), "a")))
	answer := &atomic.Value{}
	answer.Store("deny")
	prompts := &atomic.Int32{}
	var lastOptions []string
	b.Hub.Fallback = func(_ context.Context, r approval.Request) (string, error) {
		prompts.Add(1)
		lastOptions = nil
		for _, o := range r.Options {
			lastOptions = append(lastOptions, o.Name)
		}
		return answer.Load().(string), nil
	}
	var persisted []string
	b.OnPersist = func(id config.Identity) {
		persisted = append(persisted, id.Target)
		c := b.Config()
		c.Proxy.AllowedHosts = append(c.Proxy.AllowedHosts, id.Target)
	}
	p := New(b, nil)
	p.DialUpstream = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, echo) // every host resolves to the echo server
	}
	t.Cleanup(func() {
		if len(lastOptions) > 0 && lastOptions[len(lastOptions)-1] != "always" {
			t.Errorf("network prompts must offer Always, got %v", lastOptions)
		}
	})
	return p, answer, prompts, &persisted
}

func roundtrip(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "SSH-2.0-test\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "SSH-2.0-test\n" {
		t.Fatalf("tunnel broken: %q %v", line, err)
	}
}

func TestSOCKSAsksDeniesAndAlways(t *testing.T) {
	p, answer, prompts, persisted := gate(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.ServeSOCKS(ctx, addr)
	time.Sleep(50 * time.Millisecond)

	if _, err := DialSOCKS(addr, "gitlab.com", "22"); err == nil || !strings.Contains(err.Error(), "iris denied") {
		t.Fatalf("deny: %v", err)
	}
	answer.Store("always")
	c, err := DialSOCKS(addr, "gitlab.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	roundtrip(t, c)
	c.Close()
	if len(*persisted) != 1 || (*persisted)[0] != "gitlab.com" {
		t.Fatalf("always not persisted: %v", *persisted)
	}
	// Persisted: no more prompts, even after the grant would be gone.
	p.B.Grants.RevokeAll()
	before := prompts.Load()
	c, err = DialSOCKS(addr, "gitlab.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// Built-in defaults never prompt.
	c, err = DialSOCKS(addr, "github.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if prompts.Load() != before {
		t.Fatalf("unexpected prompts: %d -> %d", before, prompts.Load())
	}
}

func TestHTTPConnectAsks(t *testing.T) {
	p, answer, prompts, _ := gate(t)
	ps := &http.Server{Handler: p}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	go ps.Serve(l)
	defer ps.Close()
	pu, _ := url.Parse("http://" + l.Addr().String())

	connect := func() (*http.Response, net.Conn) {
		c, _ := net.Dial("tcp", pu.Host)
		io.WriteString(c, "CONNECT gitlab.com:443 HTTP/1.1\r\nHost: gitlab.com:443\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp, c
	}
	resp, c := connect()
	c.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("deny should 403, got %d", resp.StatusCode)
	}
	answer.Store("15m")
	resp, c = connect()
	defer c.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("allow should 200, got %d", resp.StatusCode)
	}
	if prompts.Load() != 2 {
		t.Fatalf("prompts %d", prompts.Load())
	}
}
