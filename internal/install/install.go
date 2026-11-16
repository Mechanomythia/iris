package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/proxy"
)

type Result struct {
	SettingsPath string
	Backup       string
	Files        []string
	Warnings     []string
	LaunchAgent  string
	Changes      int
}

type InstallOptions struct {
	Options
	DryRun     bool
	NoLaunchd  bool
	ConfigPath string
}

// Binaries resolves iris and iris-helper next to the running executable.
func Binaries() (iris, helper string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	helper = filepath.Join(filepath.Dir(exe), "iris-helper")
	if _, err := os.Stat(helper); err != nil {
		return exe, "", fmt.Errorf("iris-helper not found next to %s", exe)
	}
	return exe, helper, nil
}

// EnsureConfig writes a starter config if none exists.
func EnsureConfig(path string) (created bool, err error) {
	if path == "" {
		path = paths.ConfigFile()
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(StarterConfig), 0o600)
}

func Install(o InstallOptions) (*Result, error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	if _, err := EnsureConfig(o.ConfigPath); err != nil {
		return nil, err
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	res := &Result{SettingsPath: paths.ClaudeSettings()}

	if cfg.Proxy.Enabled && !o.DryRun {
		if _, err := proxy.EnsureCA(paths.CACert(), paths.CAKey()); err != nil {
			return nil, fmt.Errorf("creating proxy CA: %w", err)
		}
	}

	// Undo a previous install first so re-running is idempotent and picks up
	// config changes (e.g. a newly added AWS identity adds AWS_CONFIG_FILE).
	doc, err := ReadSettings(res.SettingsPath)
	if err != nil {
		return nil, err
	}
	if prev, err := LoadManifest(paths.InstallManifest()); err == nil && prev.SettingsPath == res.SettingsPath {
		Revert(doc, prev)
	}
	m := &Manifest{SettingsPath: res.SettingsPath, InstalledAt: time.Now().UTC()}
	ed := &Editor{D: doc, M: m}
	if err := Plan(ed, cfg, o.Options); err != nil {
		return nil, err
	}
	res.Changes = len(m.Scalars) + len(m.ArrayAdds)
	if o.DryRun {
		return res, nil
	}

	files, err := Generate(cfg, o.Options)
	res.Files = files
	var w *Warnings
	if errors.As(err, &w) {
		res.Warnings = append(res.Warnings, w.List...)
	} else if err != nil {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(res.SettingsPath), 0o700); err != nil {
		return res, err
	}
	if b, err := os.ReadFile(res.SettingsPath); err == nil {
		res.Backup = res.SettingsPath + ".iris-backup-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(res.Backup, b, 0o600); err != nil {
			return res, err
		}
	}
	if err := WriteSettings(res.SettingsPath, doc); err != nil {
		return res, err
	}

	if runtime.GOOS == "darwin" && !o.NoLaunchd {
		if err := InstallLaunchAgent(o.IrisBin); err != nil {
			res.Warnings = append(res.Warnings, "launchd: "+err.Error())
		} else {
			m.LaunchAgent = paths.LaunchAgentPlist()
			res.LaunchAgent = m.LaunchAgent
		}
	}
	return res, SaveManifest(paths.InstallManifest(), m)
}

type UninstallOptions struct {
	Purge bool // also delete ~/.config/iris (config, CA)
}

func Uninstall(o UninstallOptions) ([]string, error) {
	var notes []string
	m, err := LoadManifest(paths.InstallManifest())
	if err != nil {
		return nil, fmt.Errorf("no install manifest at %s: %w", paths.InstallManifest(), err)
	}
	doc, err := ReadSettings(m.SettingsPath)
	if err != nil {
		return nil, err
	}
	notes = append(notes, Revert(doc, m)...)
	if err := WriteSettings(m.SettingsPath, doc); err != nil {
		return notes, err
	}
	notes = append(notes, "reverted Iris changes in "+m.SettingsPath)
	if m.LaunchAgent != "" {
		_ = UninstallLaunchAgent()
		notes = append(notes, "removed launch agent")
	}
	_ = os.RemoveAll(paths.ClaudeDir())
	_ = os.Remove(paths.InstallManifest())
	if o.Purge {
		_ = os.RemoveAll(paths.ConfigDir())
		_ = os.RemoveAll(paths.StateDir())
		notes = append(notes, "purged config, CA and state")
	}
	return notes, nil
}

// ---- launchd ----

func plist(irisBin string) string {
	path := os.Getenv("PATH")
	for _, p := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if !strings.Contains(":"+path+":", ":"+p+":") {
			path += ":" + p
		}
	}
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		return r.Replace(s)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>daemon</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ProcessType</key><string>Interactive</string>
	<key>EnvironmentVariables</key>
	<dict><key>PATH</key><string>%s</string></dict>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, paths.LaunchAgentLabel, esc(irisBin), esc(path), esc(paths.DaemonLog()), esc(paths.DaemonLog()))
}

func InstallLaunchAgent(irisBin string) error {
	p := paths.LaunchAgentPlist()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(plist(irisBin)), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+paths.LaunchAgentLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func UninstallLaunchAgent() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+paths.LaunchAgentLabel).Run()
	return os.Remove(paths.LaunchAgentPlist())
}
