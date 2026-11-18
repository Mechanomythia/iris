package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/broker"
	"github.com/mechanomythia/iris/internal/config"
)

// An expired token (401) is refreshed on the host and the request replayed
// once, body included; a token that stays dead shows up as a warning.
func TestRefreshAfter401(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte("stale"), 0o600)

	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization")+" "+string(b))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			http.Error(w, `{"error":"invalid_token"}`, 401)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upAddr := upstream.Listener.Addr().String()

	cfg := config.Default()
	cfg.Proxy.Enabled = true
	cfg.Identities = []config.Identity{{Tool: "http", Target: "api.test", Source: config.Source{File: tokFile},
		Refresh:    []string{"sh", "-c", "printf fresh > " + tokFile},
		RejectHint: "log in again"}}
	b := broker.New(cfg, "", audit.Open(filepath.Join(dir, "a.jsonl")))
	b.Hub.Fallback = func(context.Context, approval.Request) (string, error) { return "15m", nil }
	ca, _, err := EnsureCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"), []string{"api.test"})
	if err != nil {
		t.Fatal(err)
	}
	p := New(b, ca)
	p.Logf = t.Logf
	upPool := x509.NewCertPool()
	upPool.AddCert(upstream.Certificate())
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upAddr)
	}
	p.Transport = &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{RootCAs: upPool, ServerName: "example.com"}}
	ps := httptest.NewServer(p)
	defer ps.Close()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	pu, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}}}

	resp, err := client.Post("https://api.test/merge_requests", "application/json", strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("got %d %q, upstream saw %v", resp.StatusCode, body, seen)
	}
	if len(seen) != 2 || seen[0] != `Bearer stale {"title":"x"}` || seen[1] != `Bearer fresh {"title":"x"}` {
		t.Fatalf("upstream saw %v", seen)
	}
	if w := b.Warnings(); len(w) != 0 {
		t.Fatalf("warnings after success: %v", w)
	}

	// Now the token dies for good: no refresh loop, a warning instead.
	os.WriteFile(tokFile, []byte("dead"), 0o600)
	b.RefreshAfterReject(context.Background(), cfg.Identities[0]) // drop the cache (refresh is rate-limited: no-op)
	seen = nil
	resp, err = client.Get("https://api.test/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || len(seen) != 1 {
		t.Fatalf("got %d, upstream saw %v (want one try, no loop)", resp.StatusCode, seen)
	}
	w := b.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "api.test rejected your token (401): log in again") {
		t.Fatalf("warnings %v", w)
	}
}
