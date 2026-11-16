package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/install"
	"github.com/mechanomythia/iris/internal/paths"
)

// ---------------- iris claude: config dirs (aliases) ----------------

const claudeUsage = `iris claude <command>   manage which Claude Code config dirs Iris configures

  list                 dirs in your Iris config (and whether Iris is installed there)
  detect [--add]       find dirs from ~/.claude*, shell rc files (CLAUDE_CONFIG_DIR=...), $CLAUDE_CONFIG_DIR
  add <dir>...         add dirs and re-run install
  remove <dir>...      remove dirs and take Iris out of their settings

Each dir gets its own profile name (~/.claude-work -> claude-work). It shows in
popups and the audit log, and grants are kept per profile.
`

func cmdClaude(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(claudeUsage)
		return nil
	}
	switch args[0] {
	case "list":
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		st := install.LoadState()
		implicit := len(cfg.Claude.ConfigDirs) == 0
		for _, d := range install.ClaudeDirs(cfg) {
			mark := "not installed"
			if _, ok := st.Targets[paths.ClaudeSettingsIn(d)]; ok {
				mark = "installed"
			}
			note := ""
			if implicit {
				note = " (default; no claude.configDirs set)"
			}
			fmt.Printf("  %-18s %-32s %s%s\n", paths.Profile(d), paths.Tilde(d), mark, note)
		}
		return nil
	case "detect":
		fs := flag.NewFlagSet("claude detect", flag.ExitOnError)
		add := fs.Bool("add", false, "add every existing dir found and re-run install")
		_ = fs.Parse(args[1:])
		cands := install.DetectClaudeDirs()
		var dirs []string
		for _, c := range cands {
			state := "exists"
			if !c.Exists {
				state = "missing"
			} else {
				dirs = append(dirs, c.Dir)
			}
			fmt.Printf("  %-18s %-32s %-8s from %s\n", c.Profile, paths.Tilde(c.Dir), state, strings.Join(c.Reasons, ", "))
		}
		if !*add {
			fmt.Println("\nAdd them with `iris claude detect --add` or `iris claude add <dir>`.")
			return nil
		}
		if err := addClaudeDirs(dirs, false); err != nil {
			return err
		}
		return runInstall(false, false, nil, false)
	case "add":
		if len(args) < 2 {
			return errors.New("usage: iris claude add <dir>...")
		}
		if err := addClaudeDirs(args[1:], false); err != nil {
			return err
		}
		return runInstall(false, false, nil, false)
	case "remove", "rm":
		if len(args) < 2 {
			return errors.New("usage: iris claude remove <dir>...")
		}
		if err := removeClaudeDirs(args[1:]); err != nil {
			return err
		}
		return runInstall(false, false, nil, false) // install reverts dirs no longer listed
	}
	fmt.Print(claudeUsage)
	return fmt.Errorf("unknown command %q", args[0])
}

// addClaudeDirs persists dirs into claude.configDirs. When the list was
// empty (implicit default), the default dir is kept so adding a work alias
// doesn't silently drop ~/.claude.
func addClaudeDirs(dirs []string, dry bool) error {
	if _, err := install.EnsureConfig(""); err != nil {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	cur := install.ClaudeDirs(cfg)
	have := map[string]bool{}
	for _, d := range cur {
		have[d] = true
	}
	list := cur
	for _, d := range dirs {
		d = paths.Expand(d)
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a directory (start that Claude alias once first so it creates it)", d)
		}
		if !have[d] {
			have[d] = true
			list = append(list, d)
			fmt.Printf("adding Claude config dir %s (profile %s)\n", paths.Tilde(d), paths.Profile(d))
		}
	}
	if dry {
		return nil
	}
	cfg.Claude.ConfigDirs = tildeAll(list)
	return config.Save("", cfg)
}

func removeClaudeDirs(dirs []string) error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, d := range dirs {
		drop[paths.Expand(d)] = true
	}
	var keep []string
	for _, d := range install.ClaudeDirs(cfg) {
		if !drop[d] {
			keep = append(keep, d)
		}
	}
	if len(keep) == 0 {
		return errors.New("that would remove every Claude config dir; use `iris uninstall` instead")
	}
	cfg.Claude.ConfigDirs = tildeAll(keep)
	return config.Save("", cfg)
}

func tildeAll(ds []string) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = paths.Tilde(d)
	}
	return out
}

// ---------------- iris link: put iris on PATH ----------------

func lookPathIris() (string, error) { return exec.LookPath("iris") }

// userPATH asks the login shell, because GUI apps (Iris.app) get a minimal PATH.
func userPATH() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, sh, "-l", "-i", "-c", `printf %s "$PATH"`).Output()
	if err != nil || len(out) == 0 {
		return os.Getenv("PATH")
	}
	return os.Getenv("PATH") + ":" + strings.TrimSpace(string(out))
}

var pathCache string

func onPath(dir string) bool {
	if pathCache == "" {
		pathCache = userPATH()
	}
	for _, p := range filepath.SplitList(pathCache) {
		if filepath.Clean(paths.Expand(p)) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".iris-w")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func cmdLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	dir := fs.String("dir", "", "where to put the symlink (default: first writable of ~/.local/bin, /opt/homebrew/bin, /usr/local/bin on PATH)")
	addPath := fs.Bool("add-to-path", false, "append the dir to your shell rc if it isn't on PATH")
	_ = fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)

	target := *dir
	if target == "" {
		local := filepath.Join(paths.Home(), ".local", "bin")
		for _, d := range []string{local, "/opt/homebrew/bin", "/usr/local/bin"} {
			if onPath(d) && writable(d) {
				target = d
				break
			}
		}
		if target == "" {
			target = local
		}
	}
	target = paths.Expand(target)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	link := filepath.Join(target, "iris")
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink; remove it or pass --dir", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	if err := os.Symlink(exe, link); err != nil {
		return fmt.Errorf("%v (try --dir ~/.local/bin)", err)
	}
	fmt.Printf("linked %s -> %s\n", link, exe)

	if onPath(target) {
		fmt.Println("ready: open a new terminal (or run `hash -r`) and type `iris status`")
		return nil
	}
	rc, line := shellRC(target)
	if *addPath {
		if b, err := os.ReadFile(rc); err == nil && strings.Contains(string(b), line) {
			fmt.Printf("%s already adds it; open a new terminal\n", paths.Tilde(rc))
			return nil
		}
		f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := fmt.Fprintf(f, "\n# added by iris link\n%s\n", line); err != nil {
			return err
		}
		fmt.Printf("added %s to PATH in %s; open a new terminal\n", paths.Tilde(target), paths.Tilde(rc))
		return nil
	}
	fmt.Printf("%s is not on your PATH. Add it with:\n\n  echo '%s' >> %s\n\nor re-run `iris link --add-to-path`.\n", paths.Tilde(target), line, paths.Tilde(rc))
	return nil
}

func shellRC(dir string) (rc, line string) {
	d := strings.Replace(dir, paths.Home(), "$HOME", 1)
	sh := filepath.Base(os.Getenv("SHELL"))
	switch sh {
	case "fish":
		return filepath.Join(paths.Home(), ".config", "fish", "config.fish"), "fish_add_path " + d
	case "bash":
		return filepath.Join(paths.Home(), ".bash_profile"), `export PATH="` + d + `:$PATH"`
	default: // zsh is the macOS default
		return filepath.Join(paths.Home(), ".zshrc"), `export PATH="` + d + `:$PATH"`
	}
}
