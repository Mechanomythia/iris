// Package broker is the daemon core: grant check, prompt, mint, audit.
package broker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mechanomythia/iris/internal/access"
	"github.com/mechanomythia/iris/internal/approval"
	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/creds"
	"github.com/mechanomythia/iris/internal/grants"
	"github.com/mechanomythia/iris/internal/passthrough"
	"github.com/mechanomythia/iris/internal/protocol"
	"github.com/mechanomythia/iris/internal/secrets"
)

// Version is stamped at build time: -ldflags "-X github.com/mechanomythia/iris/internal/broker.Version=…".
var Version = "dev"

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

	autoMu sync.Mutex
	auto   []protocol.Event // recent uses without a popup

	rejMu     sync.Mutex
	rejected  map[string]rejection // identity key → last upstream rejection
	refreshed map[string]time.Time // identity key → last refresh run

	ProxyListen string
	// OnPersist saves an "Always" approval (network: host → allowedHosts).
	OnPersist func(id config.Identity)
}

type rejection struct {
	at     time.Time
	status int
	hint   string
	target string
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
	id      string // Claude Code tool_use_id: bounds a "this command" grant
}

// ProcInfo describes who asked.
type ProcInfo struct {
	PID     int
	Cwd     string
	Argv    []string
	Profile string // Claude config dir label (self-reported)
	// Access: "read"/"write" when the caller knows (the proxy, from the HTTP
	// method); empty = decide from Claude's running command.
	Access    string
	AccessWhy string
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
// shown in prompts while the command runs. Advisory only: anything in the
// sandbox can also send it, so it is never used to decide; it only labels
// prompts and scopes "this command" grants.
func (b *Broker) AddContext(command, cwd, session, profile, id string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	now := b.now()
	b.contexts = append(b.contexts, hookContext{at: now, command: command, cwd: cwd, session: session, profile: profile, id: id})
	// keep the last 16 within the command window
	cut := 0
	for i, c := range b.contexts {
		if now.Sub(c.at) > contextWindow(c) {
			cut = i + 1
		}
	}
	b.contexts = b.contexts[cut:]
	if len(b.contexts) > 16 {
		b.contexts = b.contexts[len(b.contexts)-16:]
	}
}

// EndContext is called from the PostToolUse hook: the command finished, so
// its "this command" grants end and it no longer labels prompts.
func (b *Broker) EndContext(id string) {
	if id == "" {
		return
	}
	if n := b.Grants.EndCommand(id); n > 0 {
		b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	}
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	for i, c := range b.contexts {
		if c.id == id {
			b.contexts = append(b.contexts[:i], b.contexts[i+1:]...)
			return
		}
	}
}

// contextWindow: commands with an id stay current until their PostToolUse
// (capped at 30 min); without one (older hook) only for a minute.
func contextWindow(c hookContext) time.Duration {
	if c.id != "" {
		return 30 * time.Minute
	}
	return time.Minute
}

// toolWords: commands that plausibly trigger a tool's credential helper.
var toolWords = map[string][]string{
	"kube":   {"kubectl", "helm", "k9s", "kustomize", "flux", "argocd"},
	"aws":    {"aws", "terraform", "tofu", "cdk", "sam", "eksctl"},
	"gcp":    {"gcloud", "gsutil", "bq", "terraform", "tofu"},
	"git":    {"git", "gh"},
	"docker": {"docker", "podman", "buildx"},
	"http":   {"curl", "wget", "http", "gh"},
	"ssh":    {"git", "ssh", "scp", "rsync", "sftp"},
}

// contextFor picks the running command (same profile) that mentions a word
// associated with the tool; otherwise the latest command, labelled. Returns
// the command text and its tool_use_id ("" when unknown).
func (b *Broker) contextFor(tool, profile string) (string, string) {
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	now := b.now()
	for i := len(b.contexts) - 1; i >= 0; i-- {
		c := b.contexts[i]
		if now.Sub(c.at) > contextWindow(c) {
			continue
		}
		if profile != "" && c.profile != "" && c.profile != profile {
			continue
		}
		for _, w := range toolWords[tool] {
			if containsWord(c.command, w) {
				return c.command, c.id
			}
		}
	}
	for i := len(b.contexts) - 1; i >= 0; i-- {
		c := b.contexts[i]
		if now.Sub(c.at) <= contextWindow(c) && (profile == "" || c.profile == "" || c.profile == profile) {
			return "(latest Bash command) " + c.command, c.id
		}
	}
	return "", ""
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
	p.Access, p.AccessWhy = b.accessFor(id, p)
	read := p.Access == access.Read
	for attempt := 0; attempt < 3; attempt++ {
		if g, ok := b.Grants.UseFor(id.Tool, id.Target, p.Profile, read); ok {
			b.noteAuto(id, p, g)
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
		if g, ok := b.Grants.UseFor(id.Tool, id.Target, p.Profile, read); ok {
			b.promptMu.Unlock()
			b.noteAuto(id, p, g)
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
	command, cmdID := b.contextFor(id.Tool, p.Profile)
	if cmdID != "" {
		for i := range opts {
			if opts[i].Once {
				opts[i].Label = "this command" // covers every request the command makes
			}
		}
	}
	if id.Tool == config.ToolNetwork {
		always, _ := config.ParseOption("always", 0)
		opts = append(opts, always)
	}
	timeout := cfg.Approval.Timeout.Duration
	if timeout <= 0 {
		timeout = time.Minute
	}
	if q := cfg.Approval.QuietTimeout.Duration; q > 0 && b.Hub.Quiet() {
		timeout = q // a quiet line is easier to miss than a popup
	}
	req := approval.Request{
		ID: grants.NewID(), Identity: id, Profile: p.Profile, Command: command,
		Recent: b.Grants.Recent(id.Tool, id.Target, p.Profile, 10*time.Minute),
		PID:    p.PID, Cwd: p.Cwd, Argv: p.Argv, Options: opts, TouchID: cfg.Approval.TouchID,
		Deadline: b.now().Add(timeout), Access: p.Access, AccessWhy: p.AccessWhy,
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
	if d.Option.Persist && b.OnPersist != nil {
		b.OnPersist(id)
	}
	var g *grants.Grant
	if d.Option.Reads {
		g = b.Grants.AddReads(id.Tool, id.Target, p.Profile, id.Scope, "reads "+d.Option.Name, d.Option.TTL, d.Option.Session)
	} else if d.Option.Once && cmdID != "" {
		g = b.Grants.AddForCommand(id.Tool, id.Target, p.Profile, id.Scope, "command", cmdID)
	} else {
		g = b.Grants.Add(id.Tool, id.Target, p.Profile, id.Scope, d.Option.Name, d.Option.TTL, d.Option.Once, d.Option.Session)
	}
	b.Audit.Write(audit.Entry{Event: "grant", Tool: id.Tool, Target: id.Target, Scope: id.Scope, Profile: p.Profile, GrantID: g.ID,
		Option: g.Option, PID: p.PID, Cwd: p.Cwd, Command: req.Command, Detail: "via " + d.Via})
	b.Hub.Broadcast(protocol.Event{Type: "grants_changed"})
	gg, ok := b.Grants.UseFor(id.Tool, id.Target, p.Profile, p.Access == access.Read)
	if !ok {
		return nil, nil // revoked in between; loop re-prompts
	}
	return gg, nil
}

// ApproveUse gates a use that mints nothing on our side (an SSH signature
// done by your own agent): same grants, prompt and audit as Credential.
func (b *Broker) ApproveUse(ctx context.Context, tool, target, scope string, p ProcInfo) error {
	cfg := b.Config()
	id, ok := cfg.Resolve(tool, target)
	if !ok {
		b.Audit.Write(audit.Entry{Event: "error", Tool: tool, Target: target, PID: p.PID, Detail: "not enabled"})
		return fmt.Errorf("iris: %s is not enabled (iris passthrough on %s)", tool, tool)
	}
	if scope != "" {
		id.Scope = scope
	}
	g, err := b.ensureGrant(ctx, cfg, id, p)
	if err != nil {
		return err
	}
	b.Audit.Write(audit.Entry{Event: "issue", Tool: tool, Target: target, Scope: id.Scope, Profile: p.Profile, GrantID: g.ID,
		Option: g.Option, PID: p.PID, Cwd: p.Cwd, Detail: "signed by your agent"})
	return nil
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

// RefreshAfterReject handles an upstream 401 for an injected token: it drops
// the cached token and, if the identity has a refresh command, runs it on the
// host (at most once per refreshEvery per identity, so a dead token cannot
// loop). It reports whether a retry may get a different token.
func (b *Broker) RefreshAfterReject(ctx context.Context, id config.Identity) bool {
	b.cacheMu.Lock()
	delete(b.cache, id.Key())
	b.cacheMu.Unlock()
	if len(id.Refresh) == 0 {
		return false
	}
	b.rejMu.Lock()
	if b.refreshed == nil {
		b.refreshed = map[string]time.Time{}
	}
	if last, ok := b.refreshed[id.Key()]; ok && b.now().Sub(last) < refreshEvery {
		b.rejMu.Unlock()
		return false
	}
	b.refreshed[id.Key()] = b.now()
	b.rejMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := secrets.Fetch(ctx, config.Source{Command: id.Refresh}); err != nil {
		b.Audit.Write(audit.Entry{Event: "error", Tool: id.Tool, Target: id.Target, Detail: "refresh after 401 failed: " + err.Error()})
		return false
	}
	b.Audit.Write(audit.Entry{Event: "refresh", Tool: id.Tool, Target: id.Target, Detail: "upstream said 401; ran " + strings.Join(id.Refresh, " ")})
	return true
}

const refreshEvery = 30 * time.Second

// NoteUpstream records how the upstream answered a request carrying an
// injected token: 401/403 shows a warning in the menu until a request
// succeeds again.
func (b *Broker) NoteUpstream(id config.Identity, status int, method, path string) {
	b.rejMu.Lock()
	defer b.rejMu.Unlock()
	if b.rejected == nil {
		b.rejected = map[string]rejection{}
	}
	switch {
	case status == 401 || status == 403:
		_, had := b.rejected[id.Key()]
		b.rejected[id.Key()] = rejection{at: b.now(), status: status, hint: id.RejectHint, target: id.Target}
		if !had || status == 401 {
			b.Audit.Write(audit.Entry{Event: "upstream-rejected", Tool: id.Tool, Target: id.Target,
				Detail: fmt.Sprintf("%d for %s %s", status, method, path)})
		}
	case status >= 200 && status < 400:
		delete(b.rejected, id.Key())
	}
}

// Warnings are shown in the menu.
func (b *Broker) Warnings() []string {
	b.rejMu.Lock()
	defer b.rejMu.Unlock()
	var out []string
	for _, r := range b.rejected {
		if b.now().Sub(r.at) > 6*time.Hour {
			continue
		}
		msg := fmt.Sprintf("%s rejected your token (%d)", r.target, r.status)
		if r.status == 403 {
			msg = fmt.Sprintf("%s refused a request (403): the token may lack a scope", r.target)
		}
		if r.hint != "" {
			msg += ": " + r.hint
		}
		out = append(out, msg)
	}
	sort.Strings(out)
	return out
}

// accessFor decides read or write. The caller may know (HTTP method);
// otherwise every running command that could have triggered this tool must
// be a read, so a concurrent write can't hide behind a read.
func (b *Broker) accessFor(id config.Identity, p ProcInfo) (string, string) {
	if p.Access != "" {
		return p.Access, p.AccessWhy
	}
	tool := id.Tool
	switch {
	case tool == config.ToolNetwork:
		return access.Write, "network access"
	case tool == config.ToolSSH && strings.HasPrefix(id.Target, "signature:"):
		return access.Write, "signs a commit or tag"
	case tool == config.ToolHTTP:
		return access.Write, "unknown request"
	}
	b.ctxMu.Lock()
	defer b.ctxMu.Unlock()
	now := b.now()
	var whys []string
	for _, c := range b.contexts {
		if now.Sub(c.at) > contextWindow(c) || (p.Profile != "" && c.profile != "" && c.profile != p.Profile) {
			continue
		}
		mentions := false
		for _, w := range toolWords[tool] {
			if containsWord(c.command, w) {
				mentions = true
				break
			}
		}
		if !mentions {
			continue
		}
		lvl, why := access.Command(tool, c.command)
		if lvl != access.Read {
			return access.Write, why
		}
		whys = append(whys, why)
	}
	if len(whys) == 0 {
		return access.Write, "no command reported for it"
	}
	return access.Read, strings.Join(whys, "; ")
}

// maxAuto: auto-allowed uses kept for the menu.
const maxAuto = 100

// noteAuto records a use that went through without a popup (an existing
// grant covered it) and tells the menu bar.
func (b *Broker) noteAuto(id config.Identity, p ProcInfo, g *grants.Grant) {
	detail := ""
	if len(p.Argv) > 0 && id.Tool == config.ToolHTTP {
		detail = p.Argv[0]
	} else if cmd, _ := b.contextFor(id.Tool, p.Profile); cmd != "" {
		detail = cmd
	} else if len(p.Argv) > 0 {
		detail = strings.Join(p.Argv, " ")
	}
	ev := protocol.Event{Type: "auto_allowed", ID: g.ID, Tool: id.Tool, Target: id.Target, Scope: id.Scope, Label: id.Label(),
		Profile: p.Profile, Access: p.Access, AccessWhy: p.AccessWhy, Time: b.now(), Grant: g.Option, Reads: g.Reads, Detail: detail}
	b.autoMu.Lock()
	b.auto = append(b.auto, ev)
	if len(b.auto) > maxAuto {
		b.auto = b.auto[len(b.auto)-maxAuto:]
	}
	b.autoMu.Unlock()
	b.Hub.Broadcast(ev)
}

// Auto returns the recent auto-allowed uses, newest last.
func (b *Broker) Auto() []protocol.Event {
	b.autoMu.Lock()
	defer b.autoMu.Unlock()
	return append([]protocol.Event(nil), b.auto...)
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
		ProxyListen: b.ProxyListen, PendingPrompt: b.Hub.Pending(), Warnings: b.Warnings()}
	for _, id := range cfg.Identities {
		s.Identities = append(s.Identities, id.Label())
	}
	return s
}
