package install

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
)

// Credential locations hidden from sandboxed Bash (sandbox.credentials.files)
// and from Claude's own Read tool (permissions.deny Read(...)).
var DenyFiles = []string{
	"~/.kube",
	"~/.config/gcloud",
	"~/.aws",
	"~/.ssh",
	"~/.docker/config.json",
	"~/.config/gh",
	"~/.git-credentials",
	"~/.netrc",
	"~/.npmrc",
	"~/.pypirc",
	"~/.vault-token",
	"~/.config/op",
	"~/.op",
	"~/Library/Keychains",
	"~/.config/iris", // Iris config + proxy CA key
	"~/.local/state/iris/audit.jsonl",
	"~/.local/state/iris/irisd.log",
}

// Env vars unset before every sandboxed command.
var DenyEnv = []string{
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN",
	"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_OAUTH_ACCESS_TOKEN", "CLOUDSDK_AUTH_ACCESS_TOKEN",
	"GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITLAB_TOKEN", "GL_TOKEN",
	"KUBE_TOKEN", "DOCKER_AUTH_CONFIG", "NPM_TOKEN", "VAULT_TOKEN",
	"OP_SERVICE_ACCOUNT_TOKEN", "OP_CONNECT_TOKEN",
	"DIGITALOCEAN_TOKEN", "CLOUDFLARE_API_TOKEN", "HF_TOKEN",
	"AZURE_CLIENT_SECRET", "ARM_CLIENT_SECRET",
}

type Options struct {
	IrisBin      string // absolute path to iris
	HelperBin    string // absolute path to iris-helper
	ExtraEnvDeny []string
}

// Plan builds the settings edits for a config. Pure: no I/O.
func Plan(e *Editor, cfg *config.Config, o Options) error {
	set := func(p string, v any) error { return e.Set(p, v) }
	add := func(p string, v any) error { return e.Append(p, v) }
	var errs []error
	try := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// 1. Sandbox on, no escape hatch.
	try(set("sandbox.enabled", true))
	try(set("sandbox.failIfUnavailable", true))
	try(set("sandbox.allowUnsandboxedCommands", false))

	// 2. Hide credentials.
	for _, f := range DenyFiles {
		try(add("sandbox.credentials.files", map[string]any{"path": f, "mode": "deny"}))
	}
	for _, v := range append(append([]string{}, DenyEnv...), o.ExtraEnvDeny...) {
		try(add("sandbox.credentials.envVars", map[string]any{"name": v, "mode": "deny"}))
	}
	// Claude's Read/Edit tools are not sandboxed: deny them separately.
	for _, f := range DenyFiles {
		try(add("permissions.deny", "Read("+readRule(f)+")"))
	}
	for _, f := range []string{"~/.config/iris/**", "~/.local/share/iris/**", "~/.local/state/iris/**",
		"~/.claude/settings.json", "~/Library/LaunchAgents/**"} {
		try(add("permissions.deny", "Edit("+f+")"))
	}
	// Speed bump only (string match); the real protection is that the
	// sandbox can't reach secrets. See README "Known gaps".
	try(add("permissions.deny", "Bash(security find-generic-password:*)"))
	try(add("permissions.deny", "Bash(security find-internet-password:*)"))

	// 3. Let the helper reach the broker socket (and only that one).
	try(add("sandbox.network.allowUnixSockets", paths.BrokerSocket()))

	// 4. Point tools at Claude-only configs. Your own terminal never sees these.
	env := map[string]string{
		"IRIS_SOCKET": paths.BrokerSocket(),
	}
	if len(cfg.ByTool(config.ToolKube)) > 0 {
		env["KUBECONFIG"] = paths.ClaudeKubeconfig()
	}
	if len(cfg.ByTool(config.ToolAWS)) > 0 {
		env["AWS_CONFIG_FILE"] = paths.ClaudeAWSConfig()
		env["AWS_SHARED_CREDENTIALS_FILE"] = "/dev/null"
	}
	if len(cfg.ByTool(config.ToolGCP)) > 0 {
		env["CLOUDSDK_CONFIG"] = paths.ClaudeGcloudDir()
		try(add("sandbox.filesystem.allowWrite", paths.ClaudeGcloudDir())) // gcloud writes logs/cache here
	}
	if len(cfg.ByTool(config.ToolGit)) > 0 {
		env["GIT_CONFIG_GLOBAL"] = paths.ClaudeGitconfig()
		env["GIT_TERMINAL_PROMPT"] = "0"
		env["GH_CONFIG_DIR"] = paths.ClaudeDir() + "/gh"
	}
	if len(cfg.ByTool(config.ToolDocker)) > 0 {
		env["DOCKER_CONFIG"] = paths.ClaudeDockerDir()
	}
	if cfg.Proxy.Enabled {
		_, port, err := splitHostPort(cfg.Proxy.Listen)
		if err != nil {
			try(err)
		} else {
			try(set("sandbox.network.httpProxyPort", port))
		}
		b := paths.ClaudeCABundle()
		env["SSL_CERT_FILE"] = b
		env["CURL_CA_BUNDLE"] = b
		env["REQUESTS_CA_BUNDLE"] = b
		env["NODE_EXTRA_CA_CERTS"] = b
		env["GIT_SSL_CAINFO"] = b
		env["AWS_CA_BUNDLE"] = b
		env["CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE"] = b
	}
	for k, v := range env {
		try(set("env."+k, v))
	}

	// 5. Hooks: context for the popup, PATH shims, revoke on session end.
	hook := func(event, matcher, sub string) {
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": shellQuote(o.IrisBin) + " hook " + sub, "timeout": 5}},
		}
		if matcher != "" {
			entry["matcher"] = matcher
		}
		try(add("hooks."+event, entry))
	}
	hook("PreToolUse", "Bash", "pre-tool-use")
	hook("SessionStart", "", "session-start")
	hook("SessionEnd", "", "session-end")

	if len(errs) > 0 {
		return fmt.Errorf("planning settings: %v", errs)
	}
	return nil
}

func readRule(f string) string {
	if strings.HasSuffix(f, ".json") || isLikelyFile(f) {
		return f
	}
	return f + "/**" // directory
}

func isLikelyFile(f string) bool {
	switch f {
	case "~/.git-credentials", "~/.netrc", "~/.npmrc", "~/.pypirc", "~/.vault-token",
		"~/.local/state/iris/audit.jsonl", "~/.local/state/iris/irisd.log":
		return true
	}
	return false
}

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("proxy.listen %q: want host:port", s)
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("proxy.listen %q: %w", s, err)
	}
	return s[:i], p, nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
