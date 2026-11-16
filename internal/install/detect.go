package install

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mechanomythia/iris/internal/paths"
)

// Candidate is a Claude Code config dir found on this machine.
type Candidate struct {
	Dir     string
	Profile string
	Reasons []string // where we found it
	Exists  bool
}

// rcFiles are scanned for `CLAUDE_CONFIG_DIR=...` (aliases, functions, exports).
var rcFiles = []string{
	".zshrc", ".zprofile", ".zshenv", ".bashrc", ".bash_profile", ".profile",
	".aliases", ".bash_aliases", ".zsh_aliases",
	".config/fish/config.fish", ".config/zsh/.zshrc", ".oh-my-zsh/custom/aliases.zsh",
}

var configDirRe = regexp.MustCompile(`CLAUDE_CONFIG_DIR[= ]+["']?([^"'\s;)]+)`)

// DetectClaudeDirs finds Claude config dirs from shell rc files, the
// environment, and dirs named like ~/.claude*.
func DetectClaudeDirs() []Candidate {
	found := map[string]*Candidate{}
	add := func(dir, reason string) {
		if strings.Contains(dir, "$") && !strings.Contains(dir, "$HOME") && !strings.Contains(dir, "${HOME}") {
			return // unresolvable variable
		}
		d := paths.Expand(dir)
		c, ok := found[d]
		if !ok {
			c = &Candidate{Dir: d, Profile: paths.Profile(d)}
			found[d] = c
		}
		for _, r := range c.Reasons {
			if r == reason {
				return
			}
		}
		c.Reasons = append(c.Reasons, reason)
	}

	h := paths.Home()
	add(filepath.Join(h, ".claude"), "default")
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		add(v, "$CLAUDE_CONFIG_DIR")
	}
	for _, rc := range rcFiles {
		b, err := os.ReadFile(filepath.Join(h, rc))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "#") {
				continue
			}
			for _, m := range configDirRe.FindAllStringSubmatch(t, -1) {
				add(m[1], "~/"+rc)
			}
		}
	}
	for _, pat := range []string{".claude-*", ".claude_*", ".config/claude*"} {
		ms, _ := filepath.Glob(filepath.Join(h, pat))
		for _, m := range ms {
			if looksLikeClaudeDir(m) {
				add(m, "found on disk")
			}
		}
	}

	var out []Candidate
	for _, c := range found {
		st, err := os.Stat(c.Dir)
		c.Exists = err == nil && st.IsDir()
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

func looksLikeClaudeDir(d string) bool {
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return false
	}
	for _, f := range []string{"settings.json", ".claude.json", "projects", ".credentials.json", "statsig"} {
		if _, err := os.Stat(filepath.Join(d, f)); err == nil {
			return true
		}
	}
	return false
}
