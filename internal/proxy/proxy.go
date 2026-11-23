// Package proxy is the sandbox egress proxy (sandbox.network.httpProxyPort).
//
// Hosts that have an `http` identity are TLS-terminated with the Iris CA and
// get their auth header injected after approval; the token never enters the
// sandbox. Every other host is tunnelled untouched (subject to AllowedHosts).
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mechanomythia/iris/internal/access"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/broker"
	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/creds"
)

type Proxy struct {
	B         *broker.Broker
	CA        *CA
	Transport http.RoundTripper // upstream; replaceable in tests
	Logf      func(string, ...any)
	// DialUpstream is used for untouched CONNECT tunnels; replaceable in tests.
	DialUpstream func(ctx context.Context, network, addr string) (net.Conn, error)
}

func New(b *broker.Broker, ca *CA) *Proxy {
	d := &net.Dialer{Timeout: 15 * time.Second}
	return &Proxy{B: b, CA: ca,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: false,
			TLSHandshakeTimeout: 15 * time.Second, IdleConnTimeout: 60 * time.Second},
		DialUpstream: d.DialContext,
	}
}

func (p *Proxy) logf(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

func (p *Proxy) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func hostOnly(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.ToLower(hostport)
	}
	return strings.ToLower(h)
}

func (p *Proxy) identityFor(host string) (config.Identity, bool) {
	return p.B.Config().Find(config.ToolHTTP, host)
}

// DefaultAllowed are developer hosts allowed without asking (when
// proxy.noDefaults is off): package registries, GitHub, distro mirrors.
var DefaultAllowed = []string{
	"registry.npmjs.org", "*.npmjs.org", "registry.yarnpkg.com", "*.yarnpkg.com", "jsr.io", "*.jsr.io",
	"pypi.org", "files.pythonhosted.org", "*.pythonhosted.org",
	"crates.io", "*.crates.io", "proxy.golang.org", "sum.golang.org", "*.golang.org",
	"rubygems.org", "*.rubygems.org", "repo.maven.apache.org", "repo1.maven.org", "*.gradle.org", "services.gradle.org",
	"github.com", "*.github.com", "*.githubusercontent.com", "codeload.github.com",
	"registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com",
	"nodejs.org", "deb.debian.org", "archive.ubuntu.com", "security.ubuntu.com",
}

func matchHost(pat, host string) bool {
	pat = strings.ToLower(pat)
	if ok, _ := path.Match(pat, host); ok {
		return true
	}
	// *.example.com also matches deeper subdomains
	return strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:])
}

// ErrBlocked is returned for hosts that are not allowed.
var ErrBlocked = errors.New("iris proxy: host not allowed")

// decide is the egress gate for both the HTTP proxy and SOCKS. With
// unknownHosts: ask it blocks on the iris dialog.
func (p *Proxy) decide(ctx context.Context, host, port, desc string) error {
	if _, ok := p.identityFor(host); ok {
		return nil
	}
	cfg := p.B.Config().Proxy
	if !cfg.NoDefaults {
		for _, pat := range DefaultAllowed {
			if matchHost(pat, host) {
				return nil
			}
		}
	}
	for _, pat := range cfg.AllowedHosts {
		if matchHost(pat, host) {
			return nil
		}
	}
	switch cfg.UnknownHosts {
	case "ask":
		err := p.B.ApproveUse(ctx, config.ToolNetwork, host, "network access to "+net.JoinHostPort(host, port),
			broker.ProcInfo{Argv: []string{desc}})
		if err != nil {
			return fmt.Errorf("%w: %s (%v)", ErrBlocked, host, err)
		}
		return nil
	case "deny":
		return fmt.Errorf("%w: %s", ErrBlocked, host)
	case "allow":
		return nil
	}
	if len(cfg.AllowedHosts) == 0 { // legacy: no list = allow all
		return nil
	}
	return fmt.Errorf("%w: %s", ErrBlocked, host)
}

func portOf(hostport, def string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return def
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Plain-HTTP absolute-URI proxy request.
	if !r.URL.IsAbs() {
		http.Error(w, "iris proxy: absolute URL required", http.StatusBadRequest)
		return
	}
	host := hostOnly(r.URL.Host)
	if err := p.decide(r.Context(), host, portOf(r.URL.Host, "80"), r.Method+" "+r.URL.String()); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if _, inject := p.identityFor(host); inject {
		// http://api.example.com via the proxy is upgraded to HTTPS upstream,
		// so tools that can't trust the Iris CA can still use injection
		// without the token ever travelling in clear text.
		p.forward(w, r, "https", host)
		return
	}
	p.forward(w, r, r.URL.Scheme, r.URL.Host)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	if err := p.decide(r.Context(), host, portOf(r.Host, "443"), "CONNECT "+r.Host); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	if _, inject := p.identityFor(host); !inject || p.CA == nil {
		p.tunnel(r.Context(), client, r.Host)
		return
	}

	leaf, err := p.CA.Leaf(host)
	if err != nil {
		p.logf("leaf %s: %v", host, err)
		return
	}
	tc := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		p.logf("tls handshake with client for %s: %v (does the sandbox trust the Iris CA?)", host, err)
		return
	}
	defer tc.Close()
	br := bufio.NewReader(tc)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		rw := newConnResponseWriter(tc)
		p.forward(rw, req, "https", r.Host)
		if err := rw.finish(); err != nil || req.Close || rw.closeAfter {
			return
		}
	}
}

func (p *Proxy) tunnel(ctx context.Context, client net.Conn, addr string) {
	up, err := p.DialUpstream(ctx, "tcp", addr)
	if err != nil {
		return
	}
	defer up.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(up, client); closeWrite(up) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, up); closeWrite(client) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// maxReplayBody: larger request bodies are streamed and never retried.
const maxReplayBody = 4 << 20

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(w http.ResponseWriter, in *http.Request, scheme, hostport string) {
	host := hostOnly(hostport)
	out := in.Clone(in.Context())
	out.RequestURI = ""
	out.URL.Scheme = scheme
	out.URL.Host = hostport
	if out.Host == "" {
		out.Host = hostport
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}

	id, inject := p.identityFor(host)
	if inject {
		for _, prefix := range id.BlockPaths {
			if strings.HasPrefix(in.URL.Path, prefix) {
				// Telemetry and the like: never worth a prompt or your token.
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	// A request carrying an injected token may be replayed once after a
	// 401 (token refreshed on the host), so keep small bodies in memory.
	var body []byte
	replayable := false
	if inject {
		if out.Body == nil || out.Body == http.NoBody {
			replayable = true
		} else if out.ContentLength >= 0 && out.ContentLength <= maxReplayBody {
			b, err := io.ReadAll(io.LimitReader(out.Body, maxReplayBody+1))
			_ = out.Body.Close()
			if err != nil {
				http.Error(w, "iris proxy: reading request: "+err.Error(), http.StatusBadGateway)
				return
			}
			body, replayable = b, len(b) <= maxReplayBody
		}
	}
	setBody := func() {
		if body != nil {
			out.Body = io.NopCloser(bytes.NewReader(body))
			out.ContentLength = int64(len(body))
		}
	}
	inj := func() bool {
		lvl, why := access.HTTPMethod(in.Method)
		cred, err := p.B.Credential(in.Context(), config.ToolHTTP, host, broker.ProcInfo{
			Argv: []string{in.Method + " " + scheme + "://" + hostport + in.URL.Path}, Access: lvl, AccessWhy: why})
		if err != nil {
			http.Error(w, "iris proxy: "+err.Error(), http.StatusForbidden)
			return false
		}
		hdr := id.Header
		if hdr == "" {
			hdr = "Authorization"
		}
		// Whatever the sandbox sent for this header is replaced, so a
		// placeholder like "Bearer iris" works and nothing leaks upstream.
		for _, h := range id.RemoveHeaders {
			out.Header.Del(h)
		}
		out.Header.Set(hdr, creds.HeaderValue(id, cred))
		p.B.Audit.Write(audit.Entry{Event: "proxy-inject", Tool: config.ToolHTTP, Target: host, Detail: in.Method + " " + in.URL.Path})
		return true
	}
	if inject && !inj() {
		return
	}
	setBody()

	resp, err := p.Transport.RoundTrip(out)
	if err != nil {
		http.Error(w, fmt.Sprintf("iris proxy: upstream: %v", err), http.StatusBadGateway)
		return
	}
	// A 401 drops the cached token and refreshes it on the host (if the
	// identity knows how); the next request gets the fresh one either way.
	if inject && resp.StatusCode == http.StatusUnauthorized && p.B.RefreshAfterReject(in.Context(), id) && replayable {
		// The token was stale and the host refreshed it: try once more.
		_ = resp.Body.Close()
		if !inj() {
			return
		}
		setBody()
		resp, err = p.Transport.RoundTrip(out)
		if err != nil {
			http.Error(w, fmt.Sprintf("iris proxy: upstream: %v", err), http.StatusBadGateway)
			return
		}
	}
	if inject {
		p.B.NoteUpstream(id, resp.StatusCode, in.Method, in.URL.Path)
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		// stream (SSE etc.)
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				f.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

// connResponseWriter writes an HTTP/1.1 response with chunked encoding onto a
// raw (TLS) connection inside a CONNECT tunnel.
type connResponseWriter struct {
	c          net.Conn
	bw         *bufio.Writer
	h          http.Header
	wrote      bool
	chunked    *chunkWriter
	closeAfter bool
}

func newConnResponseWriter(c net.Conn) *connResponseWriter {
	return &connResponseWriter{c: c, bw: bufio.NewWriter(c), h: http.Header{}}
}

func (r *connResponseWriter) Header() http.Header { return r.h }

func (r *connResponseWriter) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	fmt.Fprintf(r.bw, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	if r.h.Get("Content-Length") == "" {
		r.h.Set("Transfer-Encoding", "chunked")
		r.chunked = &chunkWriter{w: r.bw}
	}
	_ = r.h.Write(r.bw)
	io.WriteString(r.bw, "\r\n")
}

func (r *connResponseWriter) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if r.chunked != nil {
		return r.chunked.Write(b)
	}
	return r.bw.Write(b)
}

func (r *connResponseWriter) Flush() { _ = r.bw.Flush() }

func (r *connResponseWriter) finish() error {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if r.chunked != nil {
		io.WriteString(r.bw, "0\r\n\r\n")
	}
	return r.bw.Flush()
}

type chunkWriter struct{ w io.Writer }

func (c *chunkWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if _, err := fmt.Fprintf(c.w, "%x\r\n", len(b)); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	if err != nil {
		return n, err
	}
	_, err = io.WriteString(c.w, "\r\n")
	return n, err
}
