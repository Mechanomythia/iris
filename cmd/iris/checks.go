package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/install"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/protocol"
)

type checker struct{ fails, warns int }

func (c *checker) ok(f string, a ...any)   { fmt.Printf("  ✓ "+f+"\n", a...) }
func (c *checker) warn(f string, a ...any) { c.warns++; fmt.Printf("  ! "+f+"\n", a...) }
func (c *checker) fail(f string, a ...any) { c.fails++; fmt.Printf("  ✗ "+f+"\n", a...) }

func (c *checker) done() error {
	fmt.Printf("\n%d failed, %d warnings\n", c.fails, c.warns)
	if c.fails > 0 {
		return fmt.Errorf("%d check(s) failed", c.fails)
	}
	return nil
}

// MinClaudeVersion: sandbox.credentials landed in 2.1.187.
var MinClaudeVersion = [3]int{2, 1, 187}

func cmdDoctor() error {
	c := &checker{}
	fmt.Println("Config")
	cfg, err := config.Load("")
	if err != nil {
		c.fail("config: %v", err)
		cfg = config.Default()
	} else {
		c.ok("%s (%d identities)", paths.ConfigFile(), len(cfg.Identities))
	}
	for _, id := range cfg.Identities {
		if len(id.Source.Command) > 0 {
			if _, err := exec.LookPath(id.Source.Command[0]); err != nil {
				c.fail("%s: source command %q not on PATH", id.Key(), id.Source.Command[0])
			}
		}
		if id.Source.Op != "" {
			if _, err := exec.LookPath("op"); err != nil {
				c.fail("%s: 1Password CLI `op` not installed", id.Key())
			}
		}
	}
	if id, ok := firstHTTP(cfg); ok && !cfg.Proxy.Enabled {
		c.fail("%s is an http identity but proxy.enabled is false", id)
	}

	fmt.Println("Claude Code")
	if out, err := exec.Command("claude", "--version").Output(); err != nil {
		c.warn("`claude` not on PATH; can't check version (need >= %d.%d.%d)", MinClaudeVersion[0], MinClaudeVersion[1], MinClaudeVersion[2])
	} else if v, ok := parseVersion(string(out)); !ok {
		c.warn("could not parse claude version from %q", strings.TrimSpace(string(out)))
	} else if less(v, MinClaudeVersion) {
		c.fail("claude %d.%d.%d is too old: sandbox.credentials needs >= %d.%d.%d", v[0], v[1], v[2], MinClaudeVersion[0], MinClaudeVersion[1], MinClaudeVersion[2])
	} else {
		c.ok("claude %d.%d.%d", v[0], v[1], v[2])
	}

	doc, err := install.ReadSettings(paths.ClaudeSettings())
	if err != nil {
		c.fail("%v", err)
		doc = install.Doc{}
	}
	get := func(path string) any {
		var cur any = map[string]any(doc)
		for _, k := range strings.Split(path, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[k]
		}
		return cur
	}
	if get("sandbox.enabled") == true {
		c.ok("sandbox.enabled")
	} else {
		c.fail("sandbox.enabled is not true in %s (run iris install)", paths.ClaudeSettings())
	}
	if get("sandbox.allowUnsandboxedCommands") == false {
		c.ok("unsandboxed retry disabled")
	} else {
		c.fail("sandbox.allowUnsandboxedCommands is not false: Claude could retry outside the sandbox")
	}
	if get("sandbox.network.allowAllUnixSockets") == true {
		c.fail("sandbox.network.allowAllUnixSockets is true: the sandbox could reach control.sock and approve itself")
	}
	socks, _ := get("sandbox.network.allowUnixSockets").([]any)
	hasBroker := false
	for _, s := range socks {
		if s == paths.BrokerSocket() {
			hasBroker = true
		}
		if s == paths.ControlSocket() || (fmt.Sprint(s) != paths.BrokerSocket() && strings.HasPrefix(paths.ControlSocket(), fmt.Sprint(s))) {
			c.fail("allowUnixSockets contains %v which exposes control.sock", s)
		}
	}
	if hasBroker {
		c.ok("broker socket allowed in sandbox")
	} else {
		c.fail("broker socket not in sandbox.network.allowUnixSockets")
	}
	if ex, _ := get("sandbox.excludedCommands").([]any); len(ex) > 0 {
		c.warn("sandbox.excludedCommands runs %v outside the sandbox, where credentials are readable", ex)
	}
	if fs := get("sandbox.filesystem.disabled"); fs == true {
		c.fail("sandbox.filesystem.disabled is true: credential files are readable")
	}
	for _, scope := range []string{".claude/settings.json", ".claude/settings.local.json"} {
		if d, err := install.ReadSettings(scope); err == nil {
			if sb, ok := d["sandbox"].(map[string]any); ok {
				if sb["enabled"] == false || sb["allowUnsandboxedCommands"] == true {
					c.warn("%s (this directory) weakens the sandbox", scope)
				}
			}
		}
	}

	fmt.Println("Generated files")
	irisBin, helper, err := install.Binaries()
	if err != nil {
		c.fail("%v", err)
	} else {
		c.ok("iris %s, helper %s", irisBin, helper)
	}
	want := map[string]string{config.ToolKube: paths.ClaudeKubeconfig(), config.ToolAWS: paths.ClaudeAWSConfig(),
		config.ToolGit: paths.ClaudeGitconfig(), config.ToolGCP: filepath.Join(paths.ShimDir(), "gcloud"),
		config.ToolDocker: filepath.Join(paths.ClaudeDockerDir(), "config.json")}
	for tool, f := range want {
		if len(cfg.ByTool(tool)) == 0 {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			c.fail("%s missing (run iris install)", f)
		} else {
			c.ok("%s", f)
		}
	}
	if cfg.Proxy.Enabled {
		if _, err := os.Stat(paths.ClaudeCABundle()); err != nil {
			c.fail("CA bundle %s missing", paths.ClaudeCABundle())
		} else {
			c.ok("CA bundle %s", paths.ClaudeCABundle())
		}
	}

	fmt.Println("Daemon")
	var r protocol.Response
	if err := control(protocol.Request{Op: "status"}, &r); err != nil {
		c.fail("%v", err)
	} else {
		c.ok("irisd %s running", r.Status.Version)
		if r.Status.UIConnected {
			c.ok("Iris.app connected")
		} else {
			c.warn("Iris.app not connected: falling back to a 3-button osascript dialog without Touch ID")
		}
		if len(r.Status.Identities) != len(cfg.Identities) {
			c.warn("daemon has %d identities, config has %d: run `iris reload`", len(r.Status.Identities), len(cfg.Identities))
		}
	}
	return c.done()
}

func firstHTTP(cfg *config.Config) (string, bool) {
	ids := cfg.ByTool(config.ToolHTTP)
	if len(ids) == 0 {
		return "", false
	}
	return ids[0].Key(), true
}

var verRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func parseVersion(s string) ([3]int, bool) {
	m := verRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func less(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// cmdSelftest is meant to be run by Claude, inside its sandbox:
//
//	! iris selftest       (or ask Claude to run it)
func cmdSelftest() error {
	c := &checker{}
	if os.Getenv("CLAUDECODE") == "" {
		c.warn("CLAUDECODE is not set: run this from inside a Claude Code session (ask Claude to run `iris selftest`)")
	}
	fmt.Println("Credential files must be unreadable")
	h := paths.Home()
	probe := []string{".kube/config", ".aws/credentials", ".aws/config", ".config/gcloud/application_default_credentials.json",
		".config/gcloud/credentials.db", ".docker/config.json", ".config/gh/hosts.yml", ".git-credentials", ".netrc",
		".config/iris/config.yaml", ".config/iris/ca/ca-key.pem"}
	if m, _ := filepath.Glob(filepath.Join(h, ".ssh", "id_*")); len(m) > 0 {
		for _, p := range m {
			probe = append(probe, strings.TrimPrefix(p, h+"/"))
		}
	}
	for _, rel := range probe {
		p := filepath.Join(h, rel)
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			c.ok("~/%s blocked", rel)
			continue
		}
		buf := make([]byte, 1)
		_, rerr := f.Read(buf)
		f.Close()
		if rerr != nil {
			c.ok("~/%s blocked", rel)
		} else {
			c.fail("~/%s is READABLE from here", rel)
		}
	}

	fmt.Println("Environment must hold no secrets")
	re := regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIAL)`)
	var hits []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if re.MatchString(k) && v != "" && !strings.HasPrefix(k, "IRIS_") {
			hits = append(hits, k)
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		c.ok("no secret-looking variables")
	} else {
		c.warn("secret-looking variables present (values not shown): %s. Add them with `iris install --deny-env`", strings.Join(hits, ", "))
	}

	fmt.Println("Sockets")
	if _, err := protocol.Call(paths.BrokerSocket(), protocol.Request{Op: "ping"}, 3*time.Second); err != nil {
		c.fail("broker socket unreachable: %v", err)
	} else {
		c.ok("broker socket reachable")
	}
	if conn, err := net.DialTimeout("unix", paths.ControlSocket(), 2*time.Second); err != nil {
		c.ok("control socket unreachable (good)")
	} else {
		pc := protocol.Wrap(conn)
		_ = pc.Send(protocol.Request{Op: "status"})
		var r protocol.Response
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_ = pc.Recv(&r)
		conn.Close()
		if r.OK {
			c.fail("control socket is reachable AND answering: the sandbox could approve its own requests")
		} else {
			c.warn("control socket reachable but refused us (%s); the sandbox socket allowlist should block it", r.Error)
		}
	}

	fmt.Println("Keychain")
	if out, err := exec.Command("security", "find-generic-password", "-s", "iris").CombinedOutput(); err == nil {
		c.fail("Keychain items for service=iris are visible from here; keep Iris secrets in 1Password (op://) instead")
	} else if strings.Contains(string(out), "could not be found") {
		c.ok("no Iris keychain items visible (none stored, or not visible)")
	} else {
		c.ok("keychain not accessible")
	}

	fmt.Println(`
Manual acceptance tests (run these in this Claude session):
  kubectl get pods       -> popup; Deny fails; "15 min" makes repeats silent
  iris audit -n 5        -> run in YOUR terminal: one line per issue/deny
  iris revoke --all      -> run in YOUR terminal; next kubectl call prompts again
  curl https://<http identity host>/...   -> works, token never visible here`)
	return c.done()
}
