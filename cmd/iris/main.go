// iris: configure Claude Code, run the broker daemon, manage grants, and
// provision Claude-only identities.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mechanomythia/iris/internal/audit"
	"github.com/mechanomythia/iris/internal/broker"
	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/install"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/peer"
	"github.com/mechanomythia/iris/internal/protocol"
	"github.com/mechanomythia/iris/internal/provision"
	"github.com/mechanomythia/iris/internal/proxy"
	"github.com/mechanomythia/iris/internal/sshagent"
)

const usage = `iris: credential broker for Claude Code

Setup
  iris install [--detect] [--claude-dir D]  configure Claude Code (all config dirs) + start the broker
  iris uninstall [--claude-dir D] [--purge] revert what install did (one dir or all)
  iris passthrough on|off|status [tools]    let Claude use YOUR credentials, approved per use
  iris network on|off|status|allow|forget   iris as the sandbox's network gate (ask for unknown hosts)
  iris ca trust|untrust|status              trust the (name-constrained) iris CA for Go tools like glab
  iris claude list|detect|add|remove        manage Claude config dirs (aliases with CLAUDE_CONFIG_DIR)
  iris cloud new|show|list                  set up a Claude Code cloud environment (claude.ai/code)
  iris link [--dir ~/.local/bin]            put iris on your PATH (symlink)
  iris provision kube|gcp|aws|github|secret create a Claude-only identity (iris provision <kind> -h)
  iris doctor                               check the installation
  iris config [path|show|edit]

Day to day
  iris status                               daemon, UI, identities
  iris grants                               active grants
  iris revoke [--all | <id>]                revoke grants (Iris.app menu does this too)
  iris audit [-n 50] [--json]               audit log

Internals
  iris daemon                               run the broker (launchd does this)
  iris reload                               re-read config
  iris restart                              restart the broker (after upgrading Iris)
  iris dev sparkle-keygen|...               release tooling (auto-update signing)
  iris hook pre-tool-use|session-start|session-end   (called by Claude Code)
  iris selftest                             run INSIDE a Claude Code session: acceptance checks
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "daemon":
		err = cmdDaemon(args)
	case "status":
		err = cmdStatus()
	case "grants":
		err = cmdGrants()
	case "revoke":
		err = cmdRevoke(args)
	case "audit":
		err = cmdAudit(args)
	case "reload":
		err = control(protocol.Request{Op: "reload"}, nil)
		if err == nil {
			fmt.Println("reloaded")
		}
	case "hook":
		err = cmdHook(args)
	case "provision":
		err = cmdProvision(args)
	case "claude":
		err = cmdClaude(args)
	case "passthrough":
		err = cmdPassthrough(args)
	case "link":
		err = cmdLink(args)
	case "doctor":
		err = cmdDoctor()
	case "selftest":
		err = cmdSelftest()
	case "config":
		err = cmdConfig(args)
	case "version", "--version":
		fmt.Println("iris", broker.Version)
		var r protocol.Response
		if control(protocol.Request{Op: "status"}, &r) == nil && r.Status.Version != broker.Version {
			fmt.Printf("running broker is %s: restart it with `iris restart` (or `iris install` to point it at this binary)\n", r.Status.Version)
		}
	case "restart":
		err = cmdRestart()
	case "dev":
		err = cmdDev(args)
	case "cloud":
		err = cmdCloud(args)
	case "network":
		err = cmdNetwork(args)
	case "ca":
		err = cmdCA(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "iris:", err)
		os.Exit(1)
	}
}

// ---------------- install ----------------

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "show what would change")
	noLaunchd := fs.Bool("no-launchd", false, "don't install the launchd agent (run `iris daemon` yourself)")
	extraEnv := fs.String("deny-env", "", "extra comma-separated env vars to hide from the sandbox")
	detect := fs.Bool("detect", false, "also configure every Claude config dir found by `iris claude detect`")
	pass := fs.Bool("passthrough", false, "let Claude use your own credentials (all tools), approved per use")
	var dirs multiFlag
	fs.Var(&dirs, "claude-dir", "Claude config dir (CLAUDE_CONFIG_DIR) to configure; repeatable, saved to config")
	_ = fs.Parse(args)
	if *detect {
		for _, c := range install.DetectClaudeDirs() {
			if c.Exists {
				dirs = append(dirs, c.Dir)
			}
		}
	}
	if len(dirs) > 0 {
		if err := addClaudeDirs(dirs, *dry); err != nil {
			return err
		}
	}
	if *pass && !*dry {
		if _, err := install.EnsureConfig(""); err != nil {
			return err
		}
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		cfg.Passthrough.Tools = config.PassthroughTools
		if err := config.Save("", cfg); err != nil {
			return err
		}
	}
	return runInstall(*dry, *noLaunchd, splitList(*extraEnv), true)
}

func runInstall(dry, noLaunchd bool, extraEnv []string, verbose bool) error {
	irisBin, helper, err := install.Binaries()
	if err != nil {
		return err
	}
	created, err := install.EnsureConfig("")
	if err != nil {
		return err
	}
	if created {
		fmt.Println("created starter config:", paths.ConfigFile())
	}
	res, err := install.Install(install.InstallOptions{
		Options: install.Options{IrisBin: irisBin, HelperBin: helper, ExtraEnvDeny: extraEnv},
		DryRun:  dry, NoLaunchd: noLaunchd,
	})
	if err != nil {
		return err
	}
	if dry {
		for _, t := range res.Targets {
			fmt.Printf("dry run: %d Iris entries for profile %q in %s\n", t.Changes, t.Profile, t.SettingsPath)
		}
		for _, sp := range res.Removed {
			fmt.Println("dry run: would remove Iris from", sp)
		}
		return nil
	}
	for _, t := range res.Targets {
		fmt.Printf("Claude Code [%s]: %s (%d Iris entries)\n", t.Profile, paths.Tilde(t.SettingsPath), t.Changes)
		if verbose && t.Backup != "" {
			fmt.Println("  backup:", paths.Tilde(t.Backup))
		}
	}
	for _, sp := range res.Removed {
		fmt.Println("removed Iris from", paths.Tilde(sp), "(no longer in claude.configDirs)")
	}
	if verbose {
		for _, f := range res.Files {
			fmt.Println("  generated:", paths.Tilde(f))
		}
		if res.LaunchAgent != "" {
			fmt.Println("  launch agent:", paths.Tilde(res.LaunchAgent))
		}
	}
	for _, w := range res.Warnings {
		fmt.Println("  warning:", w)
	}
	// Tell a running daemon to pick up config changes.
	_ = control(protocol.Request{Op: "reload"}, nil)
	if verbose {
		fmt.Println("\nNext: restart running Claude Code sessions, then inside one run `iris selftest`.")
		if cands := install.DetectClaudeDirs(); len(cands) > len(res.Targets) {
			fmt.Println("Other Claude config dirs exist: see `iris claude detect` (add with `iris claude add <dir>`).")
		}
		if _, err := lookPathIris(); err != nil {
			fmt.Println("`iris` is not on your PATH: run `" + irisBin + " link`")
		}
		cfg, _ := config.Load("")
		if cfg != nil && len(cfg.Identities) == 0 && len(cfg.Passthrough.Tools) == 0 {
			fmt.Println("Claude can't use any credential yet. Use your own (approved per use): `iris passthrough on`")
			fmt.Println("or create Claude-only identities: `iris provision ...`")
		} else if cfg != nil && len(cfg.Passthrough.Tools) > 0 {
			fmt.Println("Passthrough on for", strings.Join(cfg.Passthrough.Tools, ", ")+": Claude uses your own credentials after you allow each request.")
		}
	}
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete ~/.config/iris (config, CA) and state")
	var dirs multiFlag
	fs.Var(&dirs, "claude-dir", "only remove Iris from this Claude config dir (repeatable)")
	_ = fs.Parse(args)
	if len(dirs) > 0 {
		if err := removeClaudeDirs(dirs); err != nil {
			return err
		}
	}
	notes, err := install.Uninstall(install.UninstallOptions{Purge: *purge, Dirs: dirs})
	for _, n := range notes {
		fmt.Println(n)
	}
	return err
}

// ---------------- daemon ----------------

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	_ = fs.Parse(args)
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "irisd ", log.LstdFlags)
	b := broker.New(cfg, *cfgPath, audit.Open(paths.AuditLog()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := b.Reload(); err != nil {
				logger.Printf("reload: %v", err)
			} else {
				logger.Printf("config reloaded")
			}
		}
	}()

	// "Always" on a network prompt: remember the host in proxy.allowedHosts.
	b.OnPersist = func(id config.Identity) {
		if id.Tool != config.ToolNetwork {
			return
		}
		c, err := config.Load(*cfgPath)
		if err != nil {
			logger.Printf("persist %s: %v", id.Target, err)
			return
		}
		for _, h := range c.Proxy.AllowedHosts {
			if h == id.Target {
				return
			}
		}
		c.Proxy.AllowedHosts = append(c.Proxy.AllowedHosts, id.Target)
		if err := config.Save(*cfgPath, c); err != nil {
			logger.Printf("persist %s: %v", id.Target, err)
			return
		}
		go func() { _ = b.Reload() }()
		logger.Printf("network: %s added to proxy.allowedHosts", id.Target)
	}

	if cfg.Proxy.Enabled {
		ca, _, err := proxy.EnsureCA(paths.CACert(), paths.CAKey(), cfg.HTTPHosts())
		if err != nil {
			return fmt.Errorf("proxy CA: %w", err)
		}
		p := proxy.New(b, ca)
		p.Logf = logger.Printf
		b.ProxyListen = cfg.Proxy.Listen
		go func() {
			if err := p.ListenAndServe(ctx, cfg.Proxy.Listen); err != nil {
				logger.Printf("proxy: %v", err)
			}
		}()
		logger.Printf("proxy on %s", cfg.Proxy.Listen)
		if cfg.Proxy.SocksListen != "" {
			go func() {
				if err := p.ServeSOCKS(ctx, cfg.Proxy.SocksListen); err != nil {
					logger.Printf("socks: %v", err)
				}
			}()
			logger.Printf("socks on %s", cfg.Proxy.SocksListen)
		}
	}
	// SSH agent for the sandbox (ssh passthrough): forwards approved sign
	// requests to your agent (1Password, ...). Refuses everything when ssh
	// passthrough is off, since Resolve finds no identity.
	sa := &sshagent.Server{
		Upstream: func() string { return sshagent.Upstream(b.Config().SSH.Upstream) },
		Logf:     logger.Printf,
		Peer:     sshPeer,
		Gate: func(ctx context.Context, p sshagent.Purpose, key string, pid int, argv []string) error {
			return b.ApproveUse(ctx, config.ToolSSH, p.Target(), "your SSH key ("+key+")", broker.ProcInfo{PID: pid, Argv: argv})
		},
	}
	go func() {
		if err := sa.Serve(ctx, paths.SSHAgentSocket()); err != nil {
			logger.Printf("ssh agent: %v", err)
		}
	}()
	logger.Printf("broker %s on %s, control on %s, ssh agent on %s, %d identities", broker.Version, paths.BrokerSocket(), paths.ControlSocket(), paths.SSHAgentSocket(), len(cfg.Identities))
	srv := &broker.Server{B: b, Logf: logger.Printf}
	// Dev only: lets you drive the control socket from a shell that runs under
	// Claude Code. Claude cannot set the launchd daemon's environment.
	if os.Getenv("IRIS_UNSAFE_ALLOW_CLAUDE_CONTROL") == "1" {
		srv.AllowClaudeControl = true
		logger.Printf("WARNING: control socket accepts Claude descendants (IRIS_UNSAFE_ALLOW_CLAUDE_CONTROL)")
	}
	return srv.Serve(ctx, paths.BrokerSocket(), paths.ControlSocket())
}

// sshPeer describes who is asking the agent: for `git commit` that's
// ssh-keygen, so show its parent (the git command) first.
func sshPeer(c net.Conn) (int, []string) {
	pid := peer.PID(c)
	inf, ok := peer.Lookup(pid)
	if !ok {
		return pid, nil
	}
	argv := []string{inf.Args}
	if par, ok := peer.Lookup(inf.PPID); ok && par.Args != "" {
		argv = []string{par.Args + "  →  " + inf.Args}
	}
	return pid, argv
}

// ---------------- control clients ----------------

func control(req protocol.Request, out *protocol.Response) error {
	resp, err := protocol.Call(paths.ControlSocket(), req, 10*time.Second)
	if err != nil {
		return fmt.Errorf("broker not reachable (%v). Is it running? `launchctl print gui/%d/%s` or `iris daemon`", err, os.Getuid(), paths.LaunchAgentLabel)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if out != nil {
		*out = *resp
	}
	return nil
}

func protocolReload() protocol.Request { return protocol.Request{Op: "reload"} }

func cmdStatus() error {
	var r protocol.Response
	if err := control(protocol.Request{Op: "status"}, &r); err != nil {
		return err
	}
	s := r.Status
	fmt.Printf("irisd %s, up %s\n", s.Version, time.Since(s.StartedAt).Round(time.Second))
	ui := "not connected (osascript fallback, 3 buttons, no Touch ID)"
	if s.UIConnected {
		ui = "connected"
	}
	fmt.Println("Iris.app:", ui)
	for _, w := range s.Warnings {
		fmt.Println("warning:", w)
	}
	if s.ProxyListen != "" {
		fmt.Println("proxy:", s.ProxyListen)
	}
	fmt.Println("identities:")
	for _, i := range s.Identities {
		fmt.Println("  -", i)
	}
	if len(s.Identities) == 0 {
		fmt.Println("  (none)")
	}
	return cmdGrants()
}

func cmdGrants() error {
	var r protocol.Response
	if err := control(protocol.Request{Op: "grants"}, &r); err != nil {
		return err
	}
	if len(r.Grants) == 0 {
		fmt.Println("no active grants")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTOOL\tTARGET\tSCOPE\tOPTION\tEXPIRES IN\tUSES")
	for _, g := range r.Grants {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", g.ID, g.Tool, g.Target, g.Scope, g.Option,
			time.Until(g.ExpiresAt).Round(time.Second), g.Uses)
	}
	return tw.Flush()
}

func cmdRevoke(args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	all := fs.Bool("all", false, "revoke every grant")
	_ = fs.Parse(args)
	req := protocol.Request{Op: "revoke", All: *all}
	if !*all {
		if fs.NArg() != 1 {
			return errors.New("usage: iris revoke --all | <grant-id>")
		}
		req.ID = fs.Arg(0)
	}
	var r protocol.Response
	if err := control(req, &r); err != nil {
		return err
	}
	fmt.Printf("revoked %d grant(s)\n", r.Revoked)
	return nil
}

func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	n := fs.Int("n", 50, "lines")
	raw := fs.Bool("json", false, "raw JSON lines")
	_ = fs.Parse(args)
	lines := audit.Open(paths.AuditLog()).Tail(*n) // read directly: works without the daemon
	for _, l := range lines {
		if *raw {
			fmt.Println(string(l))
			continue
		}
		var e audit.Entry
		if json.Unmarshal(l, &e) != nil {
			continue
		}
		what := strings.Trim(e.Tool+"/"+e.Target, "/")
		extra := e.Option
		if e.Command != "" {
			extra += " `" + truncate(e.Command, 60) + "`"
		}
		if e.Detail != "" {
			extra += " " + e.Detail
		}
		fmt.Printf("%s  %-12s %-28s %s\n", e.Time.Local().Format("Jan 02 15:04:05"), e.Event, what, strings.TrimSpace(extra))
	}
	return nil
}

// ---------------- hooks (run by Claude Code, outside the sandbox) ----------------

func cmdHook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	profile := fs.String("profile", "", "Claude config dir label")
	if len(args) == 0 {
		return errors.New("usage: iris hook pre-tool-use|post-tool-use|session-start|session-end [--profile name]")
	}
	event := args[0]
	_ = fs.Parse(args[1:])
	var in struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		ToolUseID string `json:"tool_use_id"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	_ = json.Unmarshal(b, &in)
	// Hooks must never block or fail Claude: errors are swallowed.
	switch event {
	case "pre-tool-use":
		if in.ToolName == "Bash" && in.ToolInput.Command != "" {
			_, _ = protocol.Call(paths.BrokerSocket(), protocol.Request{Op: "context", Command: in.ToolInput.Command,
				Cwd: in.Cwd, SessionID: in.SessionID, Profile: *profile, ToolUseID: in.ToolUseID}, 2*time.Second)
		}
	case "post-tool-use":
		// The command finished: its "this command" approvals end.
		if in.ToolName == "Bash" && in.ToolUseID != "" {
			_, _ = protocol.Call(paths.BrokerSocket(), protocol.Request{Op: "context-end", ToolUseID: in.ToolUseID, Profile: *profile}, 2*time.Second)
		}
	case "session-start":
		if cfg, err := config.Load(""); err == nil {
			if note := sessionNote(cfg); note != "" {
				out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
					"hookEventName": "SessionStart", "additionalContext": note}})
				fmt.Println(string(out))
			}
		}
		if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" {
			var sb strings.Builder
			fmt.Fprintf(&sb, "export PATH=%q:\"$PATH\"\n", paths.ShimDir())
			if cfg, err := config.Load(""); err == nil {
				if ids := cfg.ByTool(config.ToolGCP); len(ids) > 0 {
					fmt.Fprintf(&sb, "export IRIS_GCP_DEFAULT_PROJECT=%q\n", ids[0].Target)
				}
			}
			if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = fh.WriteString(sb.String())
				fh.Close()
			}
		}
	case "session-end":
		_, _ = protocol.Call(paths.BrokerSocket(), protocol.Request{Op: "revoke-session", Profile: *profile}, 1*time.Second)
	default:
		return fmt.Errorf("unknown hook %q", event)
	}
	return nil
}

// sessionNote tells Claude how iris works, so it doesn't "fix" the setup by
// unsetting placeholders or hunting for tokens the sandbox hides on purpose.
func sessionNote(cfg *config.Config) string {
	var ph []string
	for _, id := range cfg.ByTool(config.ToolHTTP) {
		for k, v := range id.SandboxEnv {
			if strings.HasPrefix(v, "iris-") {
				ph = append(ph, fmt.Sprintf("%s (%s)", k, id.Target))
			}
		}
	}
	if len(cfg.Identities) == 0 && len(cfg.Passthrough.Tools) == 0 && len(ph) == 0 {
		return ""
	}
	sort.Strings(ph)
	var b strings.Builder
	b.WriteString("Credentials in this session are brokered by iris. The user approves each use in a popup on their Mac; " +
		"tokens never enter the sandbox, and the Keychain and credential files are blocked on purpose.\n")
	if len(ph) > 0 {
		fmt.Fprintf(&b, "- These variables hold iris placeholders, not real tokens: %s. The iris proxy swaps in the real token after approval. "+
			"Never unset or override them (no `env -u`, no `unset`), and don't try another login inside the sandbox: that bypasses iris and fails.\n", strings.Join(ph, ", "))
	}
	if cfg.PassthroughEnabled(config.ToolSSH) || (cfg.Proxy.Enabled && cfg.Proxy.SocksListen != "") {
		b.WriteString("- ssh and git over SSH work through iris (GIT_SSH_COMMAND); SSH is not blocked. If they fail, read the error: " +
			"`Permission denied (publickey)` means the user declined or missed the iris prompt for their key (ask them, then retry once); " +
			"`Host key verification failed` means the host is not in the user's known_hosts (ask them to run `ssh -T git@<host>` once in their terminal); " +
			"a line starting with `iris:` says exactly what to do. Don't replace GIT_SSH_COMMAND or build your own known_hosts.\n")
	}
	b.WriteString("- If a request is denied, the user declined it: don't retry with workarounds. " +
		"If a service rejects a token (401) or a credential can't be read, tell the user and suggest `iris status` / `iris audit` in their own terminal; " +
		"don't look for other credentials.\n")
	return b.String()
}

// ---------------- provision ----------------

const provisionUsage = `iris provision <kind> [flags]   (run from YOUR terminal, not from Claude)

  kube    ServiceAccount + RoleBinding; broker mints tokens with kubectl create token
  gcp     service account + roles; broker mints tokens by impersonation
  aws     IAM role + policies; broker mints creds with sts assume-role
  github  pre-filled fine-grained PAT page; token stored in Keychain
  gitlab  glab + GitLab API with your token (read from glab, injected by the proxy)
  secret  store any token in Keychain (e.g. an API key for the proxy)

Common flags: --dry-run (print commands only), --no-install (don't re-run iris install)
`

func cmdProvision(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(provisionUsage)
		return nil
	}
	kind, args := args[0], args[1:]
	fs := flag.NewFlagSet("provision "+kind, flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print the commands, change nothing")
	noInstall := fs.Bool("no-install", false, "don't regenerate Claude configs afterwards")
	ctx := context.Background()
	r := provision.NewRunner(false, os.Stdout)
	var ids []config.Identity

	switch kind {
	case "kube":
		o := provision.KubeOptions{}
		fs.StringVar(&o.Context, "context", "", "your (admin) kube context to create the ServiceAccount with (required)")
		fs.StringVar(&o.Name, "name", "", "context name Claude sees (default: same as --context)")
		fs.StringVar(&o.ClusterRole, "role", "view", "ClusterRole to bind: view, edit, admin or custom")
		fs.StringVar(&o.Namespace, "sa-namespace", "iris", "namespace for the ServiceAccount")
		fs.StringVar(&o.BindNamespace, "only-namespace", "", "bind only in this namespace (RoleBinding) instead of cluster-wide")
		fs.DurationVar(&o.TTL, "ttl", time.Hour, "token lifetime")
		_ = fs.Parse(args)
		r.DryRun = *dry
		fmt.Println("Provisioning Kubernetes identity:")
		id, err := provision.Kube(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	case "gcp":
		o := provision.GCPOptions{}
		roles := fs.String("roles", "roles/viewer", "comma-separated roles for the service account")
		fs.StringVar(&o.Project, "project", "", "GCP project (required)")
		fs.StringVar(&o.Name, "name", "", "service account id (default claude-<role>)")
		fs.StringVar(&o.User, "user", "", "principal allowed to impersonate (default: gcloud active account)")
		_ = fs.Parse(args)
		o.Roles = splitList(*roles)
		r.DryRun = *dry
		fmt.Println("Provisioning GCP identity:")
		id, err := provision.GCP(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	case "aws":
		o := provision.AWSOptions{}
		pol := fs.String("policies", "arn:aws:iam::aws:policy/ReadOnlyAccess", "comma-separated policy ARNs")
		fs.StringVar(&o.Profile, "profile", "", "your AWS profile used to create and later assume the role")
		fs.StringVar(&o.Name, "name", "claude-read", "profile name Claude sees (role is iris-<name>)")
		fs.StringVar(&o.Region, "region", "", "default region for Claude's profile")
		fs.DurationVar(&o.Duration, "ttl", time.Hour, "session duration")
		_ = fs.Parse(args)
		o.Policies = splitList(*pol)
		r.DryRun = *dry
		fmt.Println("Provisioning AWS identity:")
		id, err := provision.AWS(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	case "github":
		o := provision.GitHubOptions{}
		perms := fs.String("permissions", "contents:read,metadata:read", "comma-separated name:level, e.g. contents:write,pull_requests:write")
		fs.StringVar(&o.Name, "name", "", "token name")
		fs.StringVar(&o.Owner, "owner", "", "resource owner (user or org) to pre-select")
		fs.IntVar(&o.ExpiresDays, "expires", 30, "expiry in days")
		fs.BoolVar(&o.HTTP, "http", false, "also inject the token for api.github.com via the proxy (gh, curl)")
		fs.StringVar(&o.Host, "host", "github.com", "GitHub host (GHES)")
		fs.BoolVar(&o.NoBrowser, "no-browser", false, "print the URL only")
		_ = fs.Parse(args)
		o.Permissions = splitList(*perms)
		r.DryRun = *dry
		got, err := provision.GitHub(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, got...)
	case "gitlab":
		o := provision.GitLabOptions{}
		fs.StringVar(&o.Host, "host", "gitlab.com", "GitLab host")
		fs.BoolVar(&o.Paste, "paste", false, "store a pasted token in the Keychain instead of reading glab's")
		_ = fs.Parse(args)
		r.DryRun = *dry
		fmt.Println("Setting up GitLab (glab + API) with your token, injected by the proxy:")
		id, err := provision.GitLab(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		fmt.Println("  next: iris ca trust (glab is a Go tool and only trusts your keychain), then iris restart")
	case "secret":
		o := provision.SecretOptions{}
		fs.StringVar(&o.Tool, "tool", "http", "tool: http, git, docker, kube")
		fs.StringVar(&o.Target, "target", "", "host / registry / context (required)")
		fs.StringVar(&o.Scope, "scope", "", "label shown in the popup")
		fs.StringVar(&o.Header, "header", "", "http: header name (default Authorization)")
		fs.StringVar(&o.ValueTemplate, "value", "", `http: value template (default "Bearer {{token}}")`)
		fs.StringVar(&o.Username, "username", "", "git/docker username")
		_ = fs.Parse(args)
		r.DryRun = *dry
		id, err := provision.Secret(ctx, r, o)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	default:
		fmt.Print(provisionUsage)
		return fmt.Errorf("unknown kind %q", kind)
	}

	if *dry {
		fmt.Println("\n(dry-run) would add to", paths.ConfigFile()+":")
		for _, id := range ids {
			fmt.Println("  -", id.Label(), "←", id.Source.Describe())
		}
		return nil
	}
	if _, err := install.EnsureConfig(""); err != nil {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	for _, id := range ids {
		cfg.Upsert(id)
		fmt.Println("added identity:", id.Label())
	}
	if (len(cfg.ByTool(config.ToolHTTP)) > 0) && !cfg.Proxy.Enabled {
		cfg.Proxy.Enabled = true
		fmt.Println("enabled the egress proxy (needed for http identities)")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.Save("", cfg); err != nil {
		return err
	}
	if *noInstall {
		fmt.Println("run `iris install` to regenerate Claude configs")
		return nil
	}
	fmt.Println("updating Claude Code configuration…")
	return runInstall(false, false, nil, false)
}

// ---------------- config ----------------

func cmdConfig(args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Println(paths.ConfigFile())
	case "show":
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		fmt.Println("config:", paths.ConfigFile())
		for _, id := range cfg.Identities {
			fmt.Printf("  %-40s %s\n", id.Label(), id.Source.Describe())
		}
	case "edit":
		if _, err := install.EnsureConfig(""); err != nil {
			return err
		}
		ed := os.Getenv("EDITOR")
		if ed == "" {
			ed = "vi"
		}
		c := exec.Command(ed, paths.ConfigFile())
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
		if _, err := config.Load(""); err != nil {
			return fmt.Errorf("config is invalid: %w", err)
		}
		fmt.Println("saved. Run `iris install` to apply.")
	default:
		return errors.New("usage: iris config [path|show|edit]")
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
