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
func AuditLog() string  { return filepath.Join(StateDir(), "audit.jsonl") }
func DaemonLog() string { return filepath.Join(StateDir(), "irisd.log") }

func ShareDir() string         { return filepath.Join(home(), ".local", "share", "iris") }
func ClaudeDir() string        { return filepath.Join(ShareDir(), "claude") }
func ClaudeKubeconfig() string { return filepath.Join(ClaudeDir(), "kubeconfig") }
func ClaudeAWSConfig() string  { return filepath.Join(ClaudeDir(), "aws", "config") }
func ClaudeGcloudDir() string  { return filepath.Join(ClaudeDir(), "gcloud") }
func ClaudeGitconfig() string  { return filepath.Join(ClaudeDir(), "gitconfig") }
func ClaudeDockerDir() string  { return filepath.Join(ClaudeDir(), "docker") }
func ClaudeCABundle() string   { return filepath.Join(ClaudeDir(), "ca-bundle.pem") }
func ShimDir() string          { return filepath.Join(ClaudeDir(), "bin") }

func ClaudeSettings() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	return filepath.Join(home(), ".claude", "settings.json")
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
