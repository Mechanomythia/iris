package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/proxy"
)

// Target is one configured Claude config dir.
type TargetResult struct {
	Dir          string
	Profile      string
	SettingsPath string
	Backup       string
	Changes      int
}

type Result struct {
	Targets     []TargetResult
	Removed     []string // settings files Iris was taken out of (dir dropped from config)
	Files       []string
	Warnings    []string
	LaunchAgent string
}

type InstallOptions struct {
	Options
	DryRun     bool
	NoLaunchd  bool
	ConfigPath string
}

// State is ~/.config/iris/installed.json: one manifest per settings file.
type State struct {
	Targets     map[string]*Manifest `json:"targets"` // key: settings.json path
	LaunchAgent string               `json:"launchAgent,omitempty"`
}

func LoadState() *State {
	st := &State{Targets: map[string]*Manifest{}}
	b, err := os.ReadFile(paths.InstallManifest())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, st)
	if st.Targets == nil {
		st.Targets = map[string]*Manifest{}
	}
	// v0.1.0 wrote a single manifest at the top level.
	var old Manifest
	if json.Unmarshal(b, &old) == nil && old.SettingsPath != "" && len(st.Targets) == 0 {
		st.Targets[old.SettingsPath] = &old
		st.LaunchAgent = old.LaunchAgent
	}
	return st
}

func (st *State) Save() error {
	if len(st.Targets) == 0 && st.LaunchAgent == "" {
		err := os.Remove(paths.InstallManifest())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(paths.InstallManifest(), b, 0o600)
}

// ClaudeDirs resolves the config dirs to configure (absolute, deduplicated).
func ClaudeDirs(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range cfg.Claude.ConfigDirs {
		d = paths.Expand(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		out = []string{paths.DefaultClaudeDir()}
	}
	return out
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
	res := &Result{}
	dirs := ClaudeDirs(cfg)
	st := LoadState()

	if cfg.Proxy.Enabled && !o.DryRun {
		ca, changed, err := proxy.EnsureCA(paths.CACert(), paths.CAKey(), cfg.HTTPHosts())
		if err != nil {
			return nil, fmt.Errorf("creating proxy CA: %w", err)
		}
		if changed && len(cfg.HTTPHosts()) > 0 {
			res.Warnings = append(res.Warnings, "the iris CA was (re)created for "+strings.Join(ca.Hosts(), ", ")+
				": run `iris ca trust` (Go tools like glab and gh need it) and `iris restart`")
		}
	}

	// Every target denies Claude edits to every target's settings file, so
	// one profile can't loosen another.
	opts := o.Options
	opts.ClaudeDirs = dirs

	wanted := map[string]bool{}
	type pending struct {
		tr  TargetResult
		doc Doc
		m   *Manifest
	}
	var plans []pending
	for _, dir := range dirs {
		sp := paths.ClaudeSettingsIn(dir)
		wanted[sp] = true
		doc, err := ReadSettings(sp)
		if err != nil {
			return nil, err
		}
		// Undo the previous install first so re-running is idempotent and
		// picks up config changes.
		if prev, ok := st.Targets[sp]; ok {
			Revert(doc, prev)
		}
		m := &Manifest{SettingsPath: sp, InstalledAt: time.Now().UTC()}
		topts := opts
		topts.Profile = paths.Profile(dir)
		if err := Plan(&Editor{D: doc, M: m}, cfg, topts); err != nil {
			return nil, fmt.Errorf("%s: %w", sp, err)
		}
		plans = append(plans, pending{tr: TargetResult{Dir: dir, Profile: topts.Profile, SettingsPath: sp,
			Changes: len(m.Scalars) + len(m.ArrayAdds)}, doc: doc, m: m})
	}
	var dropped []string
	for sp := range st.Targets {
		if !wanted[sp] {
			dropped = append(dropped, sp)
		}
	}
	sort.Strings(dropped)

	if o.DryRun {
		for _, p := range plans {
			res.Targets = append(res.Targets, p.tr)
		}
		res.Removed = dropped
		return res, nil
	}

	files, err := Generate(cfg, opts)
	res.Files = files
	var w *Warnings
	if errors.As(err, &w) {
		res.Warnings = append(res.Warnings, w.List...)
	} else if err != nil {
		return res, err
	}

	for _, p := range plans {
		if err := os.MkdirAll(filepath.Dir(p.tr.SettingsPath), 0o700); err != nil {
			return res, err
		}
		if b, err := os.ReadFile(p.tr.SettingsPath); err == nil {
			p.tr.Backup = p.tr.SettingsPath + ".iris-backup-" + time.Now().Format("20060102-150405")
			if err := os.WriteFile(p.tr.Backup, b, 0o600); err != nil {
				return res, err
			}
		}
		if err := WriteSettings(p.tr.SettingsPath, p.doc); err != nil {
			return res, err
		}
		st.Targets[p.tr.SettingsPath] = p.m
		res.Targets = append(res.Targets, p.tr)
	}
	for _, sp := range dropped {
		if err := revertTarget(sp, st.Targets[sp]); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
			continue
		}
		delete(st.Targets, sp)
		res.Removed = append(res.Removed, sp)
	}

	if runtime.GOOS == "darwin" && !o.NoLaunchd {
		if err := InstallLaunchAgent(o.IrisBin); err != nil {
			res.Warnings = append(res.Warnings, "launchd: "+err.Error())
		} else {
			st.LaunchAgent = paths.LaunchAgentPlist()
			res.LaunchAgent = st.LaunchAgent
		}
	}
	return res, st.Save()
}

func revertTarget(sp string, m *Manifest) error {
	doc, err := ReadSettings(sp)
	if err != nil {
		return err
	}
	Revert(doc, m)
	return WriteSettings(sp, doc)
}

type UninstallOptions struct {
	Purge bool     // also delete ~/.config/iris (config, CA)
	Dirs  []string // only these Claude config dirs (default: all)
}

func Uninstall(o UninstallOptions) ([]string, error) {
	var notes []string
	st := LoadState()
	if len(st.Targets) == 0 && st.LaunchAgent == "" {
		return nil, fmt.Errorf("nothing installed (no %s)", paths.InstallManifest())
	}
	only := map[string]bool{}
	for _, d := range o.Dirs {
		only[paths.ClaudeSettingsIn(d)] = true
	}
	for sp, m := range st.Targets {
		if len(only) > 0 && !only[sp] {
			continue
		}
		doc, err := ReadSettings(sp)
		if err != nil {
			return notes, err
		}
		notes = append(notes, Revert(doc, m)...)
		if err := WriteSettings(sp, doc); err != nil {
			return notes, err
		}
		delete(st.Targets, sp)
		notes = append(notes, "reverted Iris changes in "+sp)
	}
	if len(st.Targets) > 0 {
		var rest []string
		for sp := range st.Targets {
			rest = append(rest, sp)
		}
		sort.Strings(rest)
		notes = append(notes, "still installed in: "+strings.Join(rest, ", ")+" (broker kept running)")
		return notes, st.Save()
	}
	if st.LaunchAgent != "" {
		_ = UninstallLaunchAgent()
		st.LaunchAgent = ""
		notes = append(notes, "removed launch agent")
	}
	_ = os.RemoveAll(paths.ClaudeDir())
	if err := st.Save(); err != nil {
		return notes, err
	}
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
	<dict><key>PATH</key><string>%s</string>%s</dict>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, paths.LaunchAgentLabel, esc(irisBin), esc(path), capturedEnv(esc), esc(paths.DaemonLog()), esc(paths.DaemonLog()))
}

// hostEnv are variables from your shell the broker needs to find YOUR
// credentials (launchd starts it with an almost empty environment).
var hostEnv = []string{"KUBECONFIG", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE",
	"CLOUDSDK_CONFIG", "DOCKER_CONFIG", "GIT_CONFIG_GLOBAL", "USE_GKE_GCLOUD_AUTH_PLUGIN"}

func capturedEnv(esc func(string) string) string {
	var b strings.Builder
	for _, k := range hostEnv {
		v := os.Getenv(k)
		// Skip values pointing at Claude's generated configs (install run from inside Claude).
		if v == "" || strings.Contains(v, paths.ClaudeDir()) {
			continue
		}
		fmt.Fprintf(&b, "<key>%s</key><string>%s</string>", k, esc(v))
	}
	return b.String()
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
