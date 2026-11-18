// Package config loads ~/.config/iris/config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mechanomythia/iris/internal/paths"
)

// Tools Iris knows how to broker.
const (
	ToolKube   = "kube"
	ToolAWS    = "aws"
	ToolGCP    = "gcp"
	ToolGit    = "git"
	ToolDocker = "docker"
	ToolHTTP   = "http"
	ToolSSH    = "ssh" // passthrough only: your SSH agent (1Password, ...)
	// ToolNetwork gates egress through the iris proxy (unknown hosts).
	ToolNetwork = "network"
)

var KnownTools = []string{ToolKube, ToolAWS, ToolGCP, ToolGit, ToolDocker, ToolHTTP, ToolSSH}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

type Config struct {
	SSH         SSH         `yaml:"ssh,omitempty"`
	Claude      Claude      `yaml:"claude"`
	Passthrough Passthrough `yaml:"passthrough"`
	Approval    Approval    `yaml:"approval"`
	Proxy       Proxy       `yaml:"proxy"`
	Identities  []Identity  `yaml:"identities"`
}

// Passthrough lets Claude use YOUR existing credentials (kube contexts, AWS
// profiles, gcloud login, git and docker credential helpers), still gated by
// the iris dialog. The sandbox keeps hiding the credential files; the broker
// resolves them on the host only after you allow a request. An explicit
// identity for the same tool/target takes precedence.
type Passthrough struct {
	Tools []string `yaml:"tools,omitempty"` // kube, aws, gcp, git, docker
}

// PassthroughTools are the tools passthrough can serve.
var PassthroughTools = []string{ToolKube, ToolAWS, ToolGCP, ToolGit, ToolDocker, ToolSSH}

// SSH configures the ssh passthrough: iris serves an SSH agent to the
// sandbox and forwards approved sign requests to your agent.
type SSH struct {
	// Upstream is your agent's socket. Empty = 1Password's agent if present,
	// else $SSH_AUTH_SOCK of the broker.
	Upstream string `yaml:"upstream,omitempty"`
}

// Claude lists the Claude Code config dirs (CLAUDE_CONFIG_DIR values) that
// `iris install` configures. Empty = just the default (~/.claude, or
// $CLAUDE_CONFIG_DIR when set).
type Claude struct {
	ConfigDirs []string `yaml:"configDirs,omitempty"`
}

type Approval struct {
	// Timeout after which an unanswered prompt counts as Deny.
	Timeout Duration `yaml:"timeout"`
	// Options offered in the popup, shortest first. Allowed: once, 15m, 1h, session, or any Go duration.
	Options []string `yaml:"options"`
	// SessionMax caps a "session" grant (it is also revoked by the SessionEnd hook).
	SessionMax Duration `yaml:"sessionMax"`
	// TouchID asks the menu bar app to require Touch ID / password before granting.
	TouchID bool `yaml:"touchID"`
}

type Proxy struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	// AllowedHosts are always allowed (glob: *.example.com), on top of the
	// built-in developer defaults (package registries, GitHub) unless
	// NoDefaults. Hosts with an http identity are always allowed.
	AllowedHosts []string `yaml:"allowedHosts"`
	// UnknownHosts decides everything else: "ask" (iris dialog, with an
	// Always option that adds the host to AllowedHosts), "deny", or "allow".
	// Empty keeps the old behaviour: allow all when AllowedHosts is empty.
	UnknownHosts string `yaml:"unknownHosts,omitempty"`
	NoDefaults   bool   `yaml:"noDefaults,omitempty"`
	// SocksListen serves SOCKS5 for non-HTTP traffic (git over SSH on port
	// 22, databases); Claude's sandbox sends it there via socksProxyPort.
	SocksListen string `yaml:"socksListen,omitempty"`
}

func (p Proxy) Asks() bool { return p.Enabled && p.UnknownHosts == "ask" }

// Identity maps (tool, target) to a secret source.
type Identity struct {
	Tool   string `yaml:"tool"`
	Target string `yaml:"target"` // kube context | aws profile | gcp project | git host | registry | http host
	Scope  string `yaml:"scope"`  // label shown in the popup: read, write, admin...
	Source Source `yaml:"source"`
	// Format of the source output. Empty = auto-detect.
	// token | exec-credential | aws-process | aws-sts | json
	Format string   `yaml:"format,omitempty"`
	MaxTTL Duration `yaml:"maxTTL,omitempty"`

	// Tool specific.
	SourceContext string `yaml:"sourceContext,omitempty"` // kube: context in your real kubeconfig to copy the cluster from (default: target)
	Namespace     string `yaml:"namespace,omitempty"`     // kube: default namespace for Claude's context
	Region        string `yaml:"region,omitempty"`        // aws
	Username      string `yaml:"username,omitempty"`      // git / docker (default x-access-token)
	Header        string `yaml:"header,omitempty"`        // http: header to inject (default Authorization)
	ValueTemplate string `yaml:"valueTemplate,omitempty"` // http: e.g. "Bearer {{token}}" (default)
	// http: headers removed before forwarding (e.g. the placeholder
	// PRIVATE-TOKEN a CLI sends when it thinks it's logged in).
	RemoveHeaders []string `yaml:"removeHeaders,omitempty"`
	// http: variables set in Claude's sandbox so the CLI believes it is
	// logged in (placeholders only; the proxy injects the real token).
	SandboxEnv map[string]string `yaml:"sandboxEnv,omitempty"`
	// http: path prefixes answered by the proxy itself (204, no token, no
	// prompt), e.g. a CLI's telemetry endpoint.
	BlockPaths []string `yaml:"blockPaths,omitempty"`
	// http: run on your Mac only when the upstream rejects the injected
	// token (401), e.g. to make glab refresh an expired OAuth token. The
	// request is then retried once with the fresh token.
	Refresh []string `yaml:"refresh,omitempty"`
	// http: what to do when the upstream keeps rejecting the token (menu).
	RejectHint string `yaml:"rejectHint,omitempty"`
	Note       string `yaml:"note,omitempty"`
}

// Source is exactly one of the fields.
type Source struct {
	Command  []string  `yaml:"command,omitempty"`  // run on the host, stdout is the secret
	Op       string    `yaml:"op,omitempty"`       // 1Password reference op://vault/item/field
	Keychain *Keychain `yaml:"keychain,omitempty"` // macOS Keychain generic password
	Env      string    `yaml:"env,omitempty"`      // env var of the daemon
	File     string    `yaml:"file,omitempty"`     // file readable by the daemon
	Static   string    `yaml:"static,omitempty"`   // tests only
	// Passthrough: resolve the user's own credential for the requested target
	// (set on identities synthesised from passthrough.tools).
	Passthrough bool `yaml:"passthrough,omitempty"`
}

type Keychain struct {
	Service string `yaml:"service"`
	Account string `yaml:"account"`
}

func (i Identity) Key() string { return i.Tool + "/" + i.Target }

func (i Identity) Label() string {
	s := i.Tool + " · " + i.Target
	if i.Scope != "" {
		s += " · " + i.Scope
	}
	return s
}

func Default() *Config {
	return &Config{
		Approval: Approval{
			Timeout:    Duration{60 * time.Second},
			Options:    []string{"once", "15m", "1h", "session"},
			SessionMax: Duration{8 * time.Hour},
			TouchID:    true,
		},
		Proxy: Proxy{Enabled: false, Listen: "127.0.0.1:18899"},
	}
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = paths.ConfigFile()
	}
	c := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.fillDefaults()
	return c, c.Validate()
}

func (c *Config) Validate() error {
	seen := map[string]bool{}
	for n, id := range c.Identities {
		where := fmt.Sprintf("identities[%d] (%s)", n, id.Key())
		if !contains(KnownTools, id.Tool) {
			return fmt.Errorf("%s: unknown tool %q (want one of %s)", where, id.Tool, strings.Join(KnownTools, ", "))
		}
		if id.Target == "" {
			return fmt.Errorf("%s: target is required", where)
		}
		if seen[id.Key()] {
			return fmt.Errorf("%s: duplicate identity", where)
		}
		seen[id.Key()] = true
		if err := id.Source.Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}
	switch c.Proxy.UnknownHosts {
	case "", "ask", "deny", "allow":
	default:
		return fmt.Errorf("proxy.unknownHosts: want ask, deny or allow, got %q", c.Proxy.UnknownHosts)
	}
	for _, t := range c.Passthrough.Tools {
		if !contains(PassthroughTools, t) {
			return fmt.Errorf("passthrough.tools: %q not supported (want %s)", t, strings.Join(PassthroughTools, ", "))
		}
	}
	if len(c.Approval.Options) == 0 {
		return errors.New("approval.options must not be empty")
	}
	for _, o := range c.Approval.Options {
		if _, err := ParseOption(o, c.Approval.SessionMax.Duration); err != nil {
			return err
		}
	}
	return nil
}

func (s Source) Validate() error {
	n := 0
	if len(s.Command) > 0 {
		n++
	}
	for _, v := range []string{s.Op, s.Env, s.File, s.Static} {
		if v != "" {
			n++
		}
	}
	if s.Keychain != nil {
		n++
	}
	if s.Passthrough {
		n++
	}
	if n != 1 {
		return fmt.Errorf("source must set exactly one of command, op, keychain, env, file, passthrough (got %d)", n)
	}
	return nil
}

func (s Source) Describe() string {
	switch {
	case len(s.Command) > 0:
		return "command: " + strings.Join(s.Command, " ")
	case s.Op != "":
		return "1password: " + s.Op
	case s.Keychain != nil:
		return "keychain: " + s.Keychain.Service + "/" + s.Keychain.Account
	case s.Env != "":
		return "env: " + s.Env
	case s.File != "":
		return "file: " + s.File
	case s.Passthrough:
		return "your own credentials (passthrough)"
	default:
		return "static"
	}
}

func (c *Config) Find(tool, target string) (Identity, bool) {
	for _, id := range c.Identities {
		if id.Tool == tool && id.Target == target {
			return id, true
		}
	}
	return Identity{}, false
}

// Resolve picks the identity for a request: an explicit identity for
// (tool, target) first, then passthrough of the user's own credential.
func (c *Config) Resolve(tool, target string) (Identity, bool) {
	if tool == ToolNetwork {
		if !c.Proxy.Asks() || ValidateTarget(target) != nil {
			return Identity{}, false
		}
		return Identity{Tool: ToolNetwork, Target: target, Scope: "network access", Source: Source{Passthrough: true}}, true
	}
	if id, ok := c.Find(tool, target); ok {
		return id, true
	}
	if !c.PassthroughEnabled(tool) || ValidateTarget(target) != nil {
		return Identity{}, false
	}
	id := Identity{Tool: tool, Target: target, Scope: "your credentials", Source: Source{Passthrough: true}}
	if tool == ToolGCP {
		// gcloud hands out cached tokens that may be close to expiry; re-fetch often.
		id.MaxTTL = Duration{10 * time.Minute}
	}
	return id, true
}

func (c *Config) PassthroughEnabled(tool string) bool { return contains(c.Passthrough.Tools, tool) }

// HTTPHosts are the hosts the proxy injects credentials for (and the only
// names the iris CA may issue certificates for).
func (c *Config) HTTPHosts() []string {
	var out []string
	for _, id := range c.ByTool(ToolHTTP) {
		out = append(out, id.Target)
	}
	return out
}

// Uses reports whether Claude should be wired up for a tool at all.
func (c *Config) Uses(tool string) bool {
	return c.PassthroughEnabled(tool) || len(c.ByTool(tool)) > 0
}

// ValidateTarget rejects targets that could smuggle flags or protocol lines
// into the host-side commands passthrough runs (the target comes from inside
// the sandbox).
func ValidateTarget(t string) error {
	if t == "" || len(t) > 256 || strings.HasPrefix(t, "-") {
		return fmt.Errorf("invalid target %q", t)
	}
	for _, r := range t {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-:@/+", r)
		if !ok {
			return fmt.Errorf("invalid character %q in target", r)
		}
	}
	return nil
}

func (c *Config) ByTool(tool string) []Identity {
	var out []Identity
	for _, id := range c.Identities {
		if id.Tool == tool {
			out = append(out, id)
		}
	}
	return out
}

// Option is one popup button.
type Option struct {
	Name string        // as configured: once, 15m, session
	TTL  time.Duration // 0 for once
	Once bool
	// Session grants are revoked by the SessionEnd hook.
	Session bool
	// Persist saves the approval (network hosts → proxy.allowedHosts).
	Persist bool
	// Label overrides the button text (once → "this command").
	Label string
}

func ParseOption(s string, sessionMax time.Duration) (Option, error) {
	switch s {
	case "once":
		return Option{Name: s, Once: true}, nil
	case "always":
		// Network only: the host is added to proxy.allowedHosts.
		return Option{Name: s, TTL: 24 * time.Hour, Persist: true}, nil
	case "session":
		if sessionMax <= 0 {
			sessionMax = 8 * time.Hour
		}
		return Option{Name: s, TTL: sessionMax, Session: true}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return Option{}, fmt.Errorf("approval option %q: want once, session or a duration like 15m", s)
	}
	return Option{Name: s, TTL: d}, nil
}

func (o Option) Display() string {
	if o.Label != "" {
		return o.Label
	}
	switch {
	case o.Once:
		return "Once"
	case o.Session:
		return "Until session end"
	case o.Persist:
		return "Always"
	}
	d := o.TTL
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return d.String()
}

// Save writes the config (used by `iris provision` to append identities).
func Save(path string, c *Config) error {
	if path == "" {
		path = paths.ConfigFile()
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Upsert replaces an identity with the same key or appends it.
func (c *Config) Upsert(id Identity) {
	for n := range c.Identities {
		if c.Identities[n].Key() == id.Key() {
			c.Identities[n] = id
			return
		}
	}
	c.Identities = append(c.Identities, id)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fillDefaults upgrades identities written by older versions, so an update
// works without re-running the provisioner.
func (c *Config) fillDefaults() {
	for n := range c.Identities {
		id := &c.Identities[n]
		if host, ok := GlabTokenHost(id.Source.Command); ok {
			if len(id.Refresh) == 0 {
				id.Refresh = GlabRefresh(host)
			}
			if id.RejectHint == "" {
				id.RejectHint = GlabRejectHint(host)
			}
		}
	}
}

// GlabTokenHost reports whether cmd is `glab config get token --host H`.
func GlabTokenHost(cmd []string) (string, bool) {
	if len(cmd) < 4 || cmd[0] != "glab" || cmd[1] != "config" || cmd[2] != "get" || cmd[3] != "token" {
		return "", false
	}
	host := "gitlab.com"
	for i := 4; i+1 < len(cmd); i++ {
		if cmd[i] == "--host" || cmd[i] == "-h" {
			host = cmd[i+1]
		}
	}
	return host, true
}

// GlabRefresh makes glab call the API once on your Mac; glab refreshes an
// expired OAuth token before it does.
func GlabRefresh(host string) []string {
	return []string{"glab", "api", "--hostname", host, "user"}
}

func GlabRejectHint(host string) string {
	return "run `glab auth login --hostname " + host + "` in your own terminal"
}
