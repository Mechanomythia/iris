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
	"path/filepath"
	"strings"
	"testing"

	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/broker"
	"github.com/mechanomythia/iris/internal/config"
)

func setup(t *testing.T, answer string) (*http.Client, *httptest.Server, *[]string) {
	t.Helper()
	dir := t.TempDir()
	var seen []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		io.WriteString(w, "hello "+r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	upHost, upPort, _ := net.SplitHostPort(upstream.Listener.Addr().String())

	cfg := config.Default()
	cfg.Proxy.Enabled = true
	cfg.Proxy.AllowedHosts = []string{"allowed.example"}
	// "api.test" is mapped to the upstream by the dialer below.
	cfg.Identities = []config.Identity{{Tool: "http", Target: "api.test", Source: config.Source{Static: "s3cret"}}}
	b := broker.New(cfg, "", audit.Open(filepath.Join(dir, "a.jsonl")))
	b.Hub.Fallback = func(context.Context, approval.Request) (string, error) { return answer, nil }

	ca, err := EnsureCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	p := New(b, ca)
	p.Logf = t.Logf
	// Upstream transport: resolve api.test to the httptest server, trust its cert.
	upPool := x509.NewCertPool()
	upPool.AddCert(upstream.Certificate())
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, _, _ := net.SplitHostPort(addr)
		if h == "api.test" {
			addr = net.JoinHostPort(upHost, upPort)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	p.Transport = &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{RootCAs: upPool, ServerName: "example.com"}}
	p.DialUpstream = dial
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	pu, _ := url.Parse(ps.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return client, ps, &seen
}

func TestInjectsHeaderOverCONNECT(t *testing.T) {
	client, _, seen := setup(t, "15m")
	req, _ := http.NewRequest("GET", "https://api.test/v1/me", nil)
	req.Header.Set("Authorization", "Bearer placeholder-from-sandbox")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello /v1/me" {
		t.Fatalf("body %q", body)
	}
	if len(*seen) != 1 || (*seen)[0] != "Bearer s3cret" {
		t.Fatalf("upstream saw %v", *seen)
	}
	// keep-alive second request on the same tunnel
	resp, err = client.Get("https://api.test/two")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*seen) != 2 {
		t.Fatalf("second request not forwarded: %v", *seen)
	}
}

func TestPlainHTTPIsUpgraded(t *testing.T) {
	client, _, seen := setup(t, "once")
	resp, err := client.Get("http://api.test/plain")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello /plain" || (*seen)[0] != "Bearer s3cret" {
		t.Fatalf("%q %v", body, *seen)
	}
}

func TestDenied(t *testing.T) {
	client, _, seen := setup(t, "deny")
	resp, err := client.Get("https://api.test/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(b), "denied") || len(*seen) != 0 {
		t.Fatalf("%d %s %v", resp.StatusCode, b, *seen)
	}
}

func TestAllowlist(t *testing.T) {
	client, _, _ := setup(t, "deny")
	_, err := client.Get("https://not-allowed.example/")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("expected proxy refusal, got %v", err)
	}
}
