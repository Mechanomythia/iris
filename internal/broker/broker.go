// Package broker is the daemon core: grant check, prompt, mint, audit.
package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/creds"
	"github.com/mechanomythia/iris/internal/grants"
	"github.com/mechanomythia/iris/internal/passthrough"
	"github.com/mechanomythia/iris/internal/protocol"
	"github.com/mechanomythia/iris/internal/secrets"
)

const Version = "0.1.0"

// ErrDenied is returned to the helper when the human says no or does not answer.
var ErrDenied = errors.New("denied by user")

type Broker struct {
	mu      sync.RWMutex
	cfg     *config.Config
	cfgPath string

	Grants  *grants.Store
	Audit   *audit.Log
	Hub     *approval.Hub
	started time.Time
	now     func() time.Time

	cacheMu sync.Mutex
	cache   map[string]*protocol.Credential

	promptMu sync.Mutex
	prompts  map[string]*prompt // coalesce concurrent prompts per identity

	ctxMu    sync.Mutex
	contexts []hookContext

	ProxyListen string
}

type prompt struct {
	done chan struct{}
}

type hookContext struct {
	at      time.Time
	command string
	cwd     string
	session string
	profile string
}

// ProcInfo describes who asked.
type ProcInfo struct {
	PID     int
	Cwd     string
	Argv    []string
	Profile string // Claude config dir label (self-reported)
}

func New(cfg *config.Config, cfgPath string, log *audit.Log) *Broker {
	return &Broker{
		cfg: cfg, cfgPath: cfgPath, Grants: grants.New(), Audit: log, Hub: approval.NewHub(),
		started: time.Now(), now: time.Now,
		cache: map[string]*protocol.Credential{}, prompts: map[string]*prompt{},
	}
}

func (b *Broker) Config() *config.Config {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cfg
}

func (b *Broker) Reload() error {
	c, err := config.Load(b.cfgPath)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.cfg = c
	b.mu.Unlock()
	b.cacheMu.Lock()
	b.cache = map[string]*protocol.Credential{}
	b.cacheMu.Unlock()
	return nil
}

// AddContext records the pending command from the PreToolUse hook. It is
// shown in the next popup within 60s. Advisory only: anything in the
// sandbox can also send it, so it is never used for decisions.
func (b *Broker) AddContext(command, cwd, session, profile string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	now := b.now()
	b.contexts = append(b.contexts, hookContext{at: now, command: command, cwd: cwd, session: session, profile: profile})
	// keep the last 16 within 60s
	cut := 0
	for i, c := range b.contexts {
		if now.Sub(c.at) > time.Minute {
			cut = i + 1
		}
	}
	b.contexts = b.contexts[cut:]
	if len(b.contexts) > 16 {
		b.contexts = b.contexts[len(b.contexts)-16:]
	}
}

// toolWords: commands that plausibly trigger a tool's credential helper.
var toolWords = map[string][]string{
	"kube":   {"kubectl", "helm", "k9s", "kustomize", "flux", "argocd"},
	"aws":    {"aws", "terraform", "tofu", "cdk", "sam", "eksctl"},
	"gcp":    {"gcloud", "gsutil", "bq", "terraform", "tofu"},
	"git":    {"git", "gh"},
	"docker": {"docker", "podman", "buildx"},
	"http":   {"curl", "wget", "http", "gh"},
}

// contextFor picks the most recent hook command (last 60s) from the same
// profile that mentions a word associated with the tool; otherwise the
// latest command, labelled.
func (b *Broker) contextFor(tool, profile string) string {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	now := b.now()
	for i := len(b.contexts) - 1; i >= 0; i-- {
		c := b.contexts[i]
		if now.Sub(c.at) > time.Minute {
			break
		}
		if profile != "" && c.profile != "" && c.profile != profile {
			continue
		}
		for _, w := range toolWords[tool] {
			if containsWord(c.command, w) {
				return c.command
			}
		}
	}
	if n := len(b.contexts); n > 0 && now.Sub(b.contexts[n-1].at) <= time.Minute {
		return "(latest Bash command) " + b.contexts[n-1].command
	}
	return ""
}

func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isWordByte(s[j-1])
		after := j+len(w) == len(s) || !isWordByte(s[j+len(w)])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isWordByte(c byte) bool {
	return c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Credential is the whole gate.
func (b *Broker) Credential(ctx context.Context, tool, target string, p ProcInfo) (*protocol.Credential, error) {
	cfg := b.Config()
	id, ok := cfg.Resolve(tool, target)
	if !ok {
		b.Audit.Write(audit.Entry{Event: "error", Tool: tool, Target: target, Profile: p.Profile, PID: p.PID, Cwd: p.Cwd, Detail: "no identity configured"})
		return nil, fmt.Errorf("iris: no identity configured for %s/%s (run `iris passthrough on` to use your own credentials, or `iris provision`)", tool, target)
	}

	g, err := b.ensureGrant(ctx, cfg, id, p)
	if err != nil {
		return nil, err
	}

	c, err := b.mint(ctx, id)
	if err != nil {
		b.Audit.Write(audit.Entry{Event: "error", Tool: tool, Target: target, Profile: p.Profile, GrantID: g.ID, PID: p.PID, Detail: err.Error()})
		return nil, fmt.Errorf("iris: minting %s failed: %w", id.Key(), err)
	}
	out := *c
	out.Fields = map[string]string{}
	for k, v := range c.Fields {
		out.Fields[k] = v
	}
	if g.ExpiresAt.Before(out.ExpiresAt) {
		out.ExpiresAt = g.ExpiresAt
	}
	detail := "expires " + out.ExpiresAt.UTC().Format(time.RFC3339)
	if out.LongLived {
		detail += "; long-lived secret (valid beyond the grant)"
	}
	if id.Source.Passthrough {
		detail += "; your credentials"
	}
	b.Audit.Write(audit.Entry{Event: "issue", Tool: tool, Target: target, Scope: id.Scope, Profile: p.Profile, GrantID: g.ID,
		Option: g.Option, PID: p.PID, Cwd: p.Cwd, Detail: detail})
	return &out, nil
}

func (b *Broker) ensureGrant(ctx context.Context, cfg *config.Config, id config.Identity, p ProcInfo) (*grants.Grant, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if g, ok := b.Grants.Use(id.Tool, id.Target, p.Profile); ok {
			return g, nil
		}
		// One prompt per identity and profile at a time; others wait for its outcome.
		pk := id.Key() + "@" + p.Profile
		b.promptMu.Lock()
		if pr, busy := b.prompts[pk]; busy {
			b.promptMu.Unlock()
			select {
			case <-pr.done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// Re-check under the lock: a prompt that finished between our first
		// check and here has already recorded its grant (ask adds the grant
		// before the prompt entry is removed), so this closes the window
		// that produced a second popup for the same request.
		if g, ok := b.Grants.Use(id.Tool, id.Target, p.Profile); ok {
			b.promptMu.Unlock()
			return g, nil
		}
		pr := &prompt{done: make(chan struct{})}
		b.prompts[pk] = pr
		b.promptMu.Unlock()

		g, err := b.ask(ctx, cfg, id, p)

		b.promptMu.Lock()
		delete(b.prompts, pk)
		b.promptMu.Unlock()
		close(pr.done)
		if err != nil {
			return nil, err
		}
		if g != nil {
			return g, nil
		}
	}
	return nil, ErrDenied
}

func (b *Broker) ask(ctx context.Context, cfg *config.Config, id config.Identity, p ProcInfo) (*grants.Grant, error) {
	var opts []config.Option
	for _, s := range cfg.Approval.Options {
		o, err := config.ParseOption(s, cfg.Approval.SessionMax.Duration)
		if err != nil {
			continue
		}
		if id.MaxTTL.Duration > 0 && o.TTL > id.MaxTTL.Duration {
			continue // never offer longer than the identity allows
		}
		opts = append(opts, o)
	}
	timeout := cfg.Approval.Timeout.Duration
	if timeout <= 0 {
		timeout = time.Minute
	}
	req := approval.Request{
		ID: grants.NewID(), Identity: id, Profile: p.Profile, Command: b.contextFor(id.Tool, p.Profile),
		PID: p.PID, Cwd: p.Cwd, Argv: p.Argv, Options: opts, TouchID: cfg.Approval.TouchID,
		Deadline: b.now().Add(timeout),
	}
	d := b.Hub.Ask(ctx, req)
	if d.Denied {
		ev := "deny"
		if d.TimedOut {
			ev = "timeout"
		}
		b.Audit.Write(audit.Entry{Event: ev, Tool: id.Tool, Target: id.Target, Scope: id.Scope, Profile: p.Profile,
			PID: p.PID, Cwd: p.Cwd, Command: req.Command, Detail: "via " + d.Via})
		return nil, ErrDenied
	}
	g := b.Grants.Add(id.Tool, id.Target, p.Profile, id.Scope, d.Option.Name, d.Option.TTL, d.Option.Once, d.Option.Session)
	b.Audit.Write(audit.Entry{Event: "grant", Tool: id.Tool, Target: id.Target, Scope: id.Scope, Profile: p.Profile, GrantID: g.ID,
		Option: g.Option, PID: p.PID, Cwd: p.Cwd, Command: req.Command, Detail: "via " + d.Via})
	b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	gg, ok := b.Grants.Use(id.Tool, id.Target, p.Profile)
	if !ok {
		return nil, nil // revoked in between; loop re-prompts
	}
	return gg, nil
}

// mint returns a cached credential or fetches a fresh one.
func (b *Broker) mint(ctx context.Context, id config.Identity) (*protocol.Credential, error) {
	now := b.now()
	b.cacheMu.Lock()
	if c, ok := b.cache[id.Key()]; ok && c.ExpiresAt.After(now.Add(30*time.Second)) {
		b.cacheMu.Unlock()
		return c, nil
	}
	b.cacheMu.Unlock()

	var raw []byte
	var err error
	if id.Source.Passthrough {
		raw, err = passthrough.Fetch(ctx, id.Tool, id.Target)
	} else {
		raw, err = secrets.Fetch(ctx, id.Source)
	}
	if err != nil {
		return nil, err
	}
	c, err := creds.Parse(id, raw, now)
	if err != nil {
		return nil, err
	}
	b.cacheMu.Lock()
	b.cache[id.Key()] = c
	b.cacheMu.Unlock()
	return c, nil
}

func (b *Broker) RevokeAll() int {
	n := b.Grants.RevokeAll()
	b.Audit.Write(audit.Entry{Event: "revoke", Detail: fmt.Sprintf("all (%d)", n)})
	b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	return n
}

func (b *Broker) Revoke(id string) int {
	n := b.Grants.Revoke(id)
	b.Audit.Write(audit.Entry{Event: "revoke", GrantID: id, Detail: fmt.Sprint(n)})
	b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	return n
}

func (b *Broker) RevokeSession(profile string) int {
	n := b.Grants.RevokeSession(profile)
	if n > 0 {
		b.Audit.Write(audit.Entry{Event: "revoke", Profile: profile, Detail: fmt.Sprintf("session end (%d)", n)})
		b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	}
	return n
}

func (b *Broker) Status() *protocol.Status {
	cfg := b.Config()
	s := &protocol.Status{Version: Version, StartedAt: b.started, UIConnected: b.Hub.UIConnected(),
		ProxyListen: b.ProxyListen, PendingPrompt: b.Hub.Pending()}
	for _, id := range cfg.Identities {
		s.Identities = append(s.Identities, id.Label())
	}
	return s
}
