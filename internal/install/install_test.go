package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
)

const userSettings = `{
  "model": "claude-opus-5-5",
  "sandbox": {"enabled": false, "network": {"allowUnixSockets": ["/var/run/docker.sock"]}},
  "permissions": {"deny": ["Read(./.env)"]},
  "env": {"FOO": "bar", "KUBECONFIG": "/mine"},
  "hooks": {"PreToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "lint"}]}]}
}`

func testCfg() *config.Config {
	c := config.Default()
	c.Proxy.Enabled = true
	c.Identities = []config.Identity{
		{Tool: "kube", Target: "staging", Source: config.Source{Static: "x"}},
		{Tool: "aws", Target: "dev", Region: "eu-west-1", Source: config.Source{Static: "x"}},
		{Tool: "gcp", Target: "proj", Source: config.Source{Static: "x"}},
		{Tool: "git", Target: "github.com", Source: config.Source{Static: "x"}},
		{Tool: "docker", Target: "ghcr.io", Source: config.Source{Static: "x"}},
	}
	return c
}

func TestPlanAndRevertRoundTrip(t *testing.T) {
	t.Setenv("IRIS_HOME", t.TempDir())
	var orig, d Doc
	_ = json.Unmarshal([]byte(userSettings), &orig)
	_ = json.Unmarshal([]byte(userSettings), &d)
	m := &Manifest{}
	if err := Plan(&Editor{D: d, M: m}, testCfg(), Options{IrisBin: "/opt/iris bin/iris", HelperBin: "/opt/iris/iris-helper"}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	s := string(b)
	for _, want := range []string{
		`"enabled":true`, `"allowUnsandboxedCommands":false`, `"failIfUnavailable":true`,
		`/var/run/docker.sock`, paths.BrokerSocket(), `"httpProxyPort":18899`,
		`{"mode":"deny","path":"~/.kube"}`, `{"mode":"deny","name":"GITHUB_TOKEN"}`,
		`"Read(~/.kube/**)"`, `"Read(~/.docker/config.json)"`, `"Read(./.env)"`,
		`"KUBECONFIG":"` + paths.ClaudeKubeconfig(), `"AWS_SHARED_CREDENTIALS_FILE":"/dev/null"`,
		`"'/opt/iris bin/iris' hook pre-tool-use"`, `"matcher":"Edit"`,
		`"model":"claude-opus-5-5"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(s, paths.ControlSocket()) {
		t.Fatal("control socket must never be allowed")
	}
	notes := Revert(d, m)
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
	if !reflect.DeepEqual(d, orig) {
		a, _ := json.MarshalIndent(d, "", " ")
		t.Fatalf("revert did not restore original:\n%s", a)
	}
}

func TestRevertKeepsUserChanges(t *testing.T) {
	t.Setenv("IRIS_HOME", t.TempDir())
	d := Doc{}
	m := &Manifest{}
	_ = Plan(&Editor{D: d, M: m}, config.Default(), Options{IrisBin: "/i", HelperBin: "/h"})
	d["sandbox"].(map[string]any)["enabled"] = false // user changed it afterwards
	notes := Revert(d, m)
	if len(notes) != 1 || !strings.Contains(notes[0], "sandbox.enabled") {
		t.Fatalf("notes = %v", notes)
	}
	if d["sandbox"].(map[string]any)["enabled"] != false {
		t.Fatal("user value overwritten")
	}
}

func TestInstallIsIdempotentAndGenerates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("IRIS_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	KubeClusterLookup = func(string) (map[string]any, error) {
		return map[string]any{"server": "https://k8s.example:6443", "certificate-authority-data": "Q0E="}, nil
	}
	os.MkdirAll(filepath.Join(home, ".config", "iris"), 0o700)
	if err := config.Save(paths.ConfigFile(), testCfg()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(paths.ClaudeSettings(), []byte(userSettings), 0o600)

	opts := InstallOptions{Options: Options{IrisBin: "/usr/local/bin/iris", HelperBin: "/usr/local/bin/iris-helper"}, NoLaunchd: true}
	r1, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(paths.ClaudeSettings())
	if _, err := Install(opts); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(paths.ClaudeSettings())
	if string(first) != string(second) {
		t.Fatal("install not idempotent")
	}
	if r1.Targets[0].Backup == "" {
		t.Fatal("no backup")
	}
	kc, _ := os.ReadFile(paths.ClaudeKubeconfig())
	for _, w := range []string{"https://k8s.example:6443", "/usr/local/bin/iris-helper", "- kube\n", "- staging", "current-context: staging"} {
		if !strings.Contains(string(kc), w) {
			t.Errorf("kubeconfig missing %q:\n%s", w, kc)
		}
	}
	aws, _ := os.ReadFile(paths.ClaudeAWSConfig())
	if !strings.Contains(string(aws), "[profile dev]\ncredential_process = /usr/local/bin/iris-helper aws dev\nregion = eu-west-1") {
		t.Errorf("aws config:\n%s", aws)
	}
	git, _ := os.ReadFile(paths.ClaudeGitconfig())
	if !strings.Contains(string(git), "helper =\n\thelper = /usr/local/bin/iris-helper git") || !strings.Contains(string(git), "insteadOf = git@github.com:") {
		t.Errorf("gitconfig:\n%s", git)
	}
	if _, err := os.Lstat(filepath.Join(paths.ShimDir(), "docker-credential-iris")); err != nil {
		t.Error("docker helper link missing")
	}
	if _, err := os.Stat(paths.CAKey()); err != nil {
		t.Error("CA not created")
	}
	if st, _ := os.Stat(paths.CAKey()); st != nil && st.Mode().Perm() != 0o600 {
		t.Errorf("CA key mode %v", st.Mode().Perm())
	}

	if _, err := Uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	var got, want Doc
	b, _ := os.ReadFile(paths.ClaudeSettings())
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(userSettings), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uninstall did not restore settings:\n%s", b)
	}
	if _, err := os.Stat(paths.ClaudeDir()); !os.IsNotExist(err) {
		t.Fatal("generated dir not removed")
	}
}

func TestMultipleClaudeDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("IRIS_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	work := filepath.Join(home, ".claude-work")
	personal := filepath.Join(home, ".claude")
	for _, d := range []string{work, personal} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(work, "settings.json"), []byte(`{"model":"x"}`), 0o600)
	// alias in .zshrc is detected
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("alias claude-work='CLAUDE_CONFIG_DIR=~/.claude-work claude'\n# CLAUDE_CONFIG_DIR=~/.ignored\n"), 0o600)
	got := DetectClaudeDirs()
	if len(got) != 2 || got[1].Dir != work || got[1].Profile != "claude-work" || !strings.Contains(strings.Join(got[1].Reasons, ","), "~/.zshrc") {
		t.Fatalf("detect: %+v", got)
	}

	cfg := testCfg()
	cfg.Claude.ConfigDirs = []string{"~/.claude", "~/.claude-work"}
	os.MkdirAll(filepath.Join(home, ".config", "iris"), 0o700)
	config.Save(paths.ConfigFile(), cfg)
	KubeClusterLookup = func(string) (map[string]any, error) { return map[string]any{"server": "https://k"}, nil }
	opts := InstallOptions{Options: Options{IrisBin: "/b/iris", HelperBin: "/b/iris-helper"}, NoLaunchd: true}
	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("targets: %+v", res.Targets)
	}
	ws, _ := os.ReadFile(filepath.Join(work, "settings.json"))
	for _, w := range []string{`"IRIS_PROFILE": "claude-work"`, `"Edit(~/.claude/settings.json)"`, `"Edit(~/.claude-work/settings.json)"`,
		`hook session-end --profile claude-work`, `"model": "x"`} {
		if !strings.Contains(string(ws), w) {
			t.Errorf("work settings missing %s:\n%s", w, ws)
		}
	}
	ps, _ := os.ReadFile(filepath.Join(personal, "settings.json"))
	if !strings.Contains(string(ps), `"IRIS_PROFILE": "claude"`) {
		t.Errorf("personal settings:\n%s", ps)
	}

	// Dropping a dir from the config takes Iris out of it on the next install.
	cfg.Claude.ConfigDirs = []string{"~/.claude"}
	config.Save(paths.ConfigFile(), cfg)
	res, err = Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != filepath.Join(work, "settings.json") {
		t.Fatalf("removed: %v", res.Removed)
	}
	ws, _ = os.ReadFile(filepath.Join(work, "settings.json"))
	if strings.TrimSpace(string(ws)) != "{\n  \"model\": \"x\"\n}" {
		t.Fatalf("work settings not restored:\n%s", ws)
	}

	// Partial uninstall keeps the rest.
	cfg.Claude.ConfigDirs = []string{"~/.claude", "~/.claude-work"}
	config.Save(paths.ConfigFile(), cfg)
	Install(opts)
	notes, err := Uninstall(UninstallOptions{Dirs: []string{work}})
	if err != nil || !strings.Contains(strings.Join(notes, "\n"), "still installed in") {
		t.Fatalf("%v %v", err, notes)
	}
	if _, err := os.Stat(paths.ClaudeDir()); err != nil {
		t.Fatal("shared generated files removed while a dir is still installed")
	}
}

func TestOldManifestFormat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("IRIS_HOME", home)
	os.MkdirAll(paths.ConfigDir(), 0o700)
	os.WriteFile(paths.InstallManifest(), []byte(`{"settingsPath":"/x/settings.json","scalars":[],"arrayAdds":[]}`), 0o600)
	if st := LoadState(); st.Targets["/x/settings.json"] == nil {
		t.Fatal("v0.1.0 manifest not migrated")
	}
}

func TestPassthroughConfigs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("IRIS_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	KubeContexts = func() ([]KubeContext, string, error) {
		return []KubeContext{
			{Name: "gke_p_europe-west4_kvasir", Namespace: "apps", Cluster: map[string]any{"server": "https://gke"}},
			{Name: "staging", Cluster: map[string]any{"server": "https://old"}},
			{Name: "bad name", Cluster: map[string]any{"server": "https://x"}},
		}, "gke_p_europe-west4_kvasir", nil
	}
	KubeClusterLookup = func(string) (map[string]any, error) { return map[string]any{"server": "https://explicit"}, nil }
	AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "default", Region: "eu-west-1"}, {Name: "prod-sso"}}, nil
	}
	GcloudProject = func() string { return "my-proj" }

	cfg := config.Default()
	cfg.Passthrough.Tools = config.PassthroughTools
	cfg.Identities = []config.Identity{{Tool: "kube", Target: "staging", Scope: "read", Source: config.Source{Static: "x"}}}
	os.MkdirAll(paths.ConfigDir(), 0o700)
	config.Save(paths.ConfigFile(), cfg)
	res, err := Install(InstallOptions{Options: Options{IrisBin: "/b/iris", HelperBin: "/b/iris-helper"}, NoLaunchd: true})
	if err != nil {
		t.Fatal(err)
	}
	kc, _ := os.ReadFile(paths.ClaudeKubeconfig())
	for _, w := range []string{"current-context: gke_p_europe-west4_kvasir", "- gke_p_europe-west4_kvasir", "namespace: apps", "https://explicit"} {
		if !strings.Contains(string(kc), w) {
			t.Errorf("kubeconfig missing %q:\n%s", w, kc)
		}
	}
	if strings.Contains(string(kc), "https://old") || strings.Contains(string(kc), "bad name") {
		t.Errorf("explicit identity should override passthrough; bad names skipped:\n%s", kc)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), `"bad name" skipped`) {
		t.Errorf("warnings: %v", res.Warnings)
	}
	aws, _ := os.ReadFile(paths.ClaudeAWSConfig())
	if !strings.Contains(string(aws), "[default]\ncredential_process = /b/iris-helper aws default\nregion = eu-west-1") || !strings.Contains(string(aws), "[profile prod-sso]") {
		t.Errorf("aws:\n%s", aws)
	}
	props, _ := os.ReadFile(filepath.Join(paths.ClaudeGcloudDir(), "properties"))
	if !strings.Contains(string(props), "project = my-proj") {
		t.Errorf("gcloud props:\n%s", props)
	}
	dc, _ := os.ReadFile(filepath.Join(paths.ClaudeDockerDir(), "config.json"))
	if !strings.Contains(string(dc), `"credsStore": "iris"`) {
		t.Errorf("docker:\n%s", dc)
	}
	git, _ := os.ReadFile(paths.ClaudeGitconfig())
	// ssh passthrough on: signing via ssh-keygen + iris agent, SSH remotes kept.
	if !strings.Contains(string(git), "[gpg \"ssh\"]\n\tprogram = ssh-keygen") || strings.Contains(string(git), "insteadOf") {
		t.Errorf("git:\n%s", git)
	}
	s0, _ := os.ReadFile(paths.ClaudeSettings())
	for _, w := range []string{paths.SSHAgentSocket(), `"SSH_AUTH_SOCK"`, "-F /dev/null", "IdentityAgent=" + paths.SSHAgentSocket(), "UserKnownHostsFile=" + paths.ClaudeKnownHosts()} {
		if !strings.Contains(string(s0), w) {
			t.Errorf("settings missing %s", w)
		}
	}
	if strings.Contains(string(s0), "1password") {
		t.Error("the 1Password socket must never be allowed in the sandbox")
	}

	// ssh passthrough off: SSH remotes are rewritten to HTTPS again.
	cfg.Passthrough.Tools = []string{"kube", "aws", "gcp", "git", "docker"}
	config.Save(paths.ConfigFile(), cfg)
	if _, err := Install(InstallOptions{Options: Options{IrisBin: "/b/iris", HelperBin: "/b/iris-helper"}, NoLaunchd: true}); err != nil {
		t.Fatal(err)
	}
	git, _ = os.ReadFile(paths.ClaudeGitconfig())
	if !strings.Contains(string(git), "insteadOf = git@github.com:") || strings.Contains(string(git), "gpg") {
		t.Errorf("git without ssh:\n%s", git)
	}
	s, _ := os.ReadFile(paths.ClaudeSettings())
	for _, w := range []string{"KUBECONFIG", "AWS_CONFIG_FILE", "CLOUDSDK_CONFIG", "GIT_CONFIG_GLOBAL", "DOCKER_CONFIG"} {
		if !strings.Contains(string(s), w) {
			t.Errorf("settings missing env %s", w)
		}
	}
}

func TestStarterConfigIsValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(StarterConfig), 0o600)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Passthrough.Tools) != 6 || !c.Approval.TouchID {
		t.Fatalf("%+v", c)
	}
	if id, ok := c.Resolve("kube", "any-context"); !ok || !id.Source.Passthrough {
		t.Fatal("passthrough resolve")
	}
	if _, ok := c.Resolve("kube", "--evil"); ok {
		t.Fatal("bad target resolved")
	}
	if _, ok := c.Resolve("http", "api.x"); ok {
		t.Fatal("http has no passthrough")
	}
}
