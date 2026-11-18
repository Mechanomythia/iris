// Package paths centralises every file location Iris uses.
//
// Layout (deliberate split between what the sandbox may and may not read):
//
//	~/.config/iris/          config, CA key, install manifest   -> DENIED to the sandbox
//	~/.local/state/iris/     sockets, audit log                 -> broker.sock reachable, rest denied
//	~/.local/share/iris/claude/  Claude-only kubeconfig, aws config, gitconfig, shims -> READABLE by the sandbox
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

func home() string {
	if h := os.Getenv("IRIS_HOME"); h != "" { // tests
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return h
}

func Home() string { return home() }

func ConfigDir() string { return filepath.Join(home(), ".config", "iris") }
func ConfigFile() string {
	if p := os.Getenv("IRIS_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(ConfigDir(), "config.yaml")
}
func CADir() string           { return filepath.Join(ConfigDir(), "ca") }
func CACert() string          { return filepath.Join(CADir(), "ca.pem") }
func CAKey() string           { return filepath.Join(CADir(), "ca-key.pem") }
func InstallManifest() string { return filepath.Join(ConfigDir(), "installed.json") }

func StateDir() string { return filepath.Join(home(), ".local", "state", "iris") }

// BrokerSocket is the only socket the sandbox may connect to. It can request
// credentials (which triggers approval) but can never approve anything.
func BrokerSocket() string {
	if p := os.Getenv("IRIS_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "broker.sock")
}

// ControlSocket is used by the menu bar app and the iris CLI to approve,
// list and revoke. It must NOT be listed in sandbox.network.allowUnixSockets.
func ControlSocket() string {
	if p := os.Getenv("IRIS_CONTROL_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(StateDir(), "control.sock")
}
func AuditLog() string { return filepath.Join(StateDir(), "audit.jsonl") }

// SSHAgentSocket is the agent the sandbox uses (ssh passthrough). Reachable
// from the sandbox; every sign request goes through the iris dialog.
func SSHAgentSocket() string   { return filepath.Join(StateDir(), "ssh-agent.sock") }
func ClaudeKnownHosts() string { return filepath.Join(ClaudeDir(), "ssh", "known_hosts") }
func DaemonLog() string        { return filepath.Join(StateDir(), "irisd.log") }

func ShareDir() string         { return filepath.Join(home(), ".local", "share", "iris") }
func ClaudeDir() string        { return filepath.Join(ShareDir(), "claude") }
func ClaudeKubeconfig() string { return filepath.Join(ClaudeDir(), "kubeconfig") }
func ClaudeAWSConfig() string  { return filepath.Join(ClaudeDir(), "aws", "config") }

// SandboxDir holds Claude-only config that tools inside the sandbox must be
// able to write (gcloud, gh). It is outside ShareDir on purpose: iris denies
// Claude edits to ShareDir, and Claude Code applies Edit deny rules to
// sandbox writes too, overriding allowWrite.
func SandboxDir() string { return filepath.Join(home(), ".local", "share", "iris-sandbox") }

func ClaudeGcloudDir() string { return filepath.Join(SandboxDir(), "gcloud") }
func ClaudeGHDir() string     { return filepath.Join(SandboxDir(), "gh") }
func ClaudeGitconfig() string { return filepath.Join(ClaudeDir(), "gitconfig") }
func ClaudeDockerDir() string { return filepath.Join(ClaudeDir(), "docker") }
func ClaudeCABundle() string  { return filepath.Join(ClaudeDir(), "ca-bundle.pem") }
func ShimDir() string         { return filepath.Join(ClaudeDir(), "bin") }

// DefaultClaudeDir is where Claude Code keeps its config when no alias
// overrides it.
func DefaultClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return Expand(d)
	}
	return filepath.Join(home(), ".claude")
}

func ClaudeSettingsIn(dir string) string { return filepath.Join(Expand(dir), "settings.json") }

// ClaudeSettings is the default config dir's settings file.
func ClaudeSettings() string { return ClaudeSettingsIn(DefaultClaudeDir()) }

// Profile names a Claude config dir for popups/audit: ~/.claude-work -> claude-work.
func Profile(dir string) string {
	b := strings.TrimPrefix(filepath.Base(filepath.Clean(Expand(dir))), ".")
	if b == "" {
		return "claude"
	}
	return b
}

// Expand resolves ~ and $HOME / ${HOME} and makes the path absolute.
func Expand(p string) string {
	p = strings.ReplaceAll(strings.ReplaceAll(p, "${HOME}", home()), "$HOME", home())
	if p == "~" {
		p = home()
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home(), p[2:])
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

// Tilde shortens a path under home to ~/... for display and permission rules.
func Tilde(p string) string {
	h := home()
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+"/") {
		return "~/" + p[len(h)+1:]
	}
	return p
}

func LaunchAgentPlist() string {
	return filepath.Join(home(), "Library", "LaunchAgents", LaunchAgentLabel+".plist")
}

const LaunchAgentLabel = "dev.iris.broker"

func EnsureDirs() error {
	for _, d := range []string{ConfigDir(), CADir(), StateDir(), ClaudeDir(), ShimDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
