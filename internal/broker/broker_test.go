package broker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/protocol"
)

type env struct {
	b        *Broker
	brokerS  string
	controlS string
	auditP   string
	prompts  atomic.Int32
	answer   atomic.Value // string
}

func start(t *testing.T) *env {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "irist")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := config.Default()
	cfg.Approval.Timeout = config.Duration{Duration: 2 * time.Second}
	cfg.Identities = []config.Identity{
		{Tool: "kube", Target: "staging", Scope: "read", Source: config.Source{Static: "kube-token"}},
		{Tool: "aws", Target: "dev", Source: config.Source{Static: `{"Version":1,"AccessKeyId":"AKIA","SecretAccessKey":"x"}`}},
	}
	e := &env{brokerS: filepath.Join(dir, "b.sock"), controlS: filepath.Join(dir, "c.sock"), auditP: filepath.Join(dir, "audit.jsonl")}
	e.answer.Store("15m")
	e.b = New(cfg, "", audit.Open(e.auditP))
	e.b.Hub.Fallback = func(ctx context.Context, r approval.Request) (string, error) {
		e.prompts.Add(1)
		a := e.answer.Load().(string)
		if a == "timeout" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return a, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := &Server{B: e.b, AllowClaudeControl: true, Logf: t.Logf}
	go func() { _ = srv.Serve(ctx, e.brokerS, e.controlS) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(e.controlS); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e
}

func (e *env) cred(t *testing.T, tool, target string) *protocol.Response {
	t.Helper()
	r, err := protocol.Call(e.brokerS, protocol.Request{Op: "credential", Tool: tool, Target: target, PID: 1, Cwd: "/x"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) auditEvents(t *testing.T) []string {
	var out []string
	for _, l := range audit.Open(e.auditP).Tail(0) {
		var a audit.Entry
		_ = json.Unmarshal(l, &a)
		out = append(out, a.Event)
	}
	return out
}

func TestGrantFlow(t *testing.T) {
	e := start(t)
	r := e.cred(t, "kube", "staging")
	if !r.OK || r.Credential.Token != "kube-token" {
		t.Fatalf("first call: %+v", r)
	}
	// Repeat within TTL: silent.
	for i := 0; i < 3; i++ {
		if r := e.cred(t, "kube", "staging"); !r.OK {
			t.Fatal(r.Error)
		}
	}
	if n := e.prompts.Load(); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	// Expiry is clamped to the grant.
	if d := time.Until(r.Credential.ExpiresAt); d > 15*time.Minute || d < 14*time.Minute {
		t.Fatalf("expiry not clamped to grant: %v", d)
	}
	// Revoke-all -> next call prompts again.
	var resp protocol.Response
	rr, err := protocol.Call(e.controlS, protocol.Request{Op: "revoke", All: true}, time.Second)
	if err != nil || !rr.OK || rr.Revoked != 1 {
		t.Fatalf("revoke: %v %+v", err, rr)
	}
	_ = resp
	e.answer.Store("deny")
	if r := e.cred(t, "kube", "staging"); r.OK || !strings.Contains(r.Error, "denied") {
		t.Fatalf("expected deny, got %+v", r)
	}
	if n := e.prompts.Load(); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
	ev := strings.Join(e.auditEvents(t), ",")
	if ev != "grant,issue,issue,issue,issue,revoke,deny" {
		t.Fatalf("audit = %s", ev)
	}
}

func TestOnceAndTimeoutAndUnknown(t *testing.T) {
	e := start(t)
	e.answer.Store("once")
	e.cred(t, "aws", "dev")
	e.cred(t, "aws", "dev")
	if n := e.prompts.Load(); n != 2 {
		t.Fatalf("once should prompt every time, got %d prompts", n)
	}
	e.answer.Store("timeout")
	if r := e.cred(t, "kube", "staging"); r.OK {
		t.Fatal("timeout must deny")
	}
	if r := e.cred(t, "kube", "nope"); r.OK || !strings.Contains(r.Error, "no identity") {
		t.Fatalf("%+v", r)
	}
	ev := e.auditEvents(t)
	if ev[len(ev)-2] != "timeout" || ev[len(ev)-1] != "error" {
		t.Fatalf("audit = %v", ev)
	}
}

func TestUnknownChoiceFailsClosed(t *testing.T) {
	e := start(t)
	e.answer.Store("forever")
	if r := e.cred(t, "kube", "staging"); r.OK {
		t.Fatal("unknown choice must deny")
	}
}

func TestConcurrentRequestsShareOnePrompt(t *testing.T) {
	e := start(t)
	done := make(chan bool, 5)
	for i := 0; i < 5; i++ {
		go func() { done <- e.cred(t, "kube", "staging").OK }()
	}
	for i := 0; i < 5; i++ {
		if !<-done {
			t.Fatal("request failed")
		}
	}
	if n := e.prompts.Load(); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
}

func TestUIApproval(t *testing.T) {
	e := start(t)
	conn, err := protocol.Call(e.controlS, protocol.Request{Op: "status"}, time.Second)
	if err != nil || conn.Status.UIConnected {
		t.Fatal("no UI expected yet")
	}
	// Connect a fake UI.
	ui, err := dialSub(e.controlS)
	if err != nil {
		t.Fatal(err)
	}
	defer ui.Close()
	go func() {
		for {
			var ev protocol.Event
			if err := ui.Recv(&ev); err != nil {
				return
			}
			if ev.Type == "approval_request" {
				if ev.Label != "kube · staging · read" || len(ev.Options) == 0 || ev.Options[0].Display != "Once" {
					t.Errorf("bad event %+v", ev)
				}
				_ = ui.Send(protocol.Request{Op: "decision", ID: ev.ID, Choice: "1h"})
			}
		}
	}()
	// context from the PreToolUse hook shows up in the request
	_, _ = protocol.Call(e.brokerS, protocol.Request{Op: "context", Command: "kubectl get pods"}, time.Second)
	if r := e.cred(t, "kube", "staging"); !r.OK {
		t.Fatal(r.Error)
	}
	if e.prompts.Load() != 0 {
		t.Fatal("fallback should not be used when a UI is connected")
	}
	g, _ := protocol.Call(e.controlS, protocol.Request{Op: "grants"}, time.Second)
	if len(g.Grants) != 1 || g.Grants[0].Option != "1h" {
		t.Fatalf("grants: %+v", g.Grants)
	}
}

func TestBrokerSocketCannotApprove(t *testing.T) {
	e := start(t)
	for _, op := range []string{"decision", "revoke", "subscribe", "reload", "grants"} {
		r, err := protocol.Call(e.brokerS, protocol.Request{Op: op, All: true, Choice: "1h"}, time.Second)
		if err != nil || r.OK {
			t.Fatalf("op %s must be refused on broker socket: %v %+v", op, err, r)
		}
	}
}

func dialSub(path string) (*protocol.Conn, error) {
	c, err := netDial(path)
	if err != nil {
		return nil, err
	}
	pc := protocol.Wrap(c)
	if err := pc.Send(protocol.Request{Op: "subscribe"}); err != nil {
		return nil, err
	}
	var r protocol.Response
	return pc, pc.Recv(&r)
}

func TestContextMatchesTool(t *testing.T) {
	b := New(config.Default(), "", audit.Open(t.TempDir()+"/a"))
	b.AddContext("kubectl get pods", "", "")
	b.AddContext("ls -la", "", "")
	if got := b.contextFor("kube"); got != "kubectl get pods" {
		t.Fatalf("kube: %q", got)
	}
	if got := b.contextFor("git"); got != "(latest Bash command) ls -la" {
		t.Fatalf("git: %q", got)
	}
	if containsWord("digit", "git") || !containsWord("cd x && git push", "git") {
		t.Fatal("word match")
	}
}
