// Package passthrough resolves the user's OWN credentials on the host, after
// the user approved the request in the iris dialog. Nothing here runs before
// approval: interactive auth flows (SSO, browser logins, keychain prompts)
// only start once you said yes.
//
// Each resolver returns raw output in a format creds.Parse understands:
// ExecCredential JSON (kube), credential_process / sts JSON (aws), or
// {"token", "username", "expires_at", "long_lived"} JSON.
package passthrough

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
)

// Runner runs a host command. Swappable in tests.
var Runner = func(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out, nil
}

// ReadFile is swappable in tests.
var ReadFile = os.ReadFile

// Fetch resolves the user's credential for tool/target.
func Fetch(ctx context.Context, tool, target string) ([]byte, error) {
	if err := config.ValidateTarget(target); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // SSO / browser flows can take a moment
	defer cancel()
	switch tool {
	case config.ToolKube:
		return Kube(ctx, target)
	case config.ToolAWS:
		return AWS(ctx, target)
	case config.ToolGCP:
		return GCP(ctx, target)
	case config.ToolGit:
		return Git(ctx, target)
	case config.ToolDocker:
		return Docker(ctx, target)
	}
	return nil, fmt.Errorf("passthrough does not support %s", tool)
}

type normalized struct {
	Token     string `json:"token"`
	Username  string `json:"username,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	LongLived bool   `json:"long_lived,omitempty"`
}

func emit(n normalized) ([]byte, error) { return json.Marshal(n) }

// ---------------- kube ----------------

type kubeUser struct {
	Token                 string `json:"token"`
	TokenFile             string `json:"tokenFile"`
	ClientCertificateData string `json:"client-certificate-data"`
	ClientKeyData         string `json:"client-key-data"`
	ClientCertificate     string `json:"client-certificate"`
	ClientKey             string `json:"client-key"`
	Username              string `json:"username"`
	Exec                  *struct {
		APIVersion string   `json:"apiVersion"`
		Command    string   `json:"command"`
		Args       []string `json:"args"`
		Env        []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"env"`
	} `json:"exec"`
	AuthProvider any `json:"auth-provider"`
}

// Kube resolves the user entry of a context in YOUR kubeconfig: a static
// token, a client certificate, or (most common: GKE, EKS, AKS, OIDC) the
// exec plugin, which is run here on the host.
func Kube(ctx context.Context, kctx string) ([]byte, error) {
	out, err := Runner(ctx, nil, nil, "kubectl", "config", "view", "--raw", "--minify", "--flatten", "-o", "json", "--context="+kctx)
	if err != nil {
		return nil, err
	}
	var kc struct {
		Users []struct {
			User kubeUser `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(kc.Users) == 0 {
		return nil, fmt.Errorf("context %s has no user", kctx)
	}
	u := kc.Users[0].User
	status := map[string]any{}
	switch {
	case u.Exec != nil:
		var env []string
		for _, e := range u.Exec.Env {
			env = append(env, e.Name+"="+e.Value)
		}
		api := u.Exec.APIVersion
		if api == "" {
			api = "client.authentication.k8s.io/v1"
		}
		info, _ := json.Marshal(map[string]any{"apiVersion": api, "kind": "ExecCredential", "spec": map[string]any{"interactive": false}})
		env = append(env, "KUBERNETES_EXEC_INFO="+string(info))
		raw, err := Runner(ctx, env, nil, u.Exec.Command, u.Exec.Args...)
		if err != nil {
			return nil, fmt.Errorf("exec plugin for %s: %w", kctx, err)
		}
		return raw, nil // ExecCredential JSON, parsed by creds
	case u.Token != "":
		status["token"] = u.Token
	case u.TokenFile != "":
		b, err := ReadFile(u.TokenFile)
		if err != nil {
			return nil, err
		}
		status["token"] = strings.TrimSpace(string(b))
	case u.ClientCertificateData != "":
		cert, err1 := base64.StdEncoding.DecodeString(u.ClientCertificateData)
		key, err2 := base64.StdEncoding.DecodeString(u.ClientKeyData)
		if err1 != nil || err2 != nil {
			return nil, errors.New("client certificate data is not base64")
		}
		status["clientCertificateData"] = string(cert)
		status["clientKeyData"] = string(key)
	case u.AuthProvider != nil:
		return nil, fmt.Errorf("context %s uses a legacy auth-provider; switch it to an exec plugin (e.g. gke-gcloud-auth-plugin)", kctx)
	default:
		return nil, fmt.Errorf("context %s has no credential iris can pass through (basic auth is not supported)", kctx)
	}
	// Static tokens and certificates don't expire on their own.
	status["iris.longLived"] = true
	return json.Marshal(map[string]any{"apiVersion": "client.authentication.k8s.io/v1", "kind": "ExecCredential", "status": status})
}

// ---------------- aws ----------------

// AWS exports the profile's credentials (works for SSO, assume-role, static
// keys). Static long-term keys are swapped for a 1h session token so the
// long-term secret never enters the sandbox; if that fails (e.g. the account
// requires MFA for GetSessionToken) the request fails rather than leaking it.
func AWS(ctx context.Context, profile string) ([]byte, error) {
	out, err := Runner(ctx, nil, nil, "aws", "configure", "export-credentials", "--profile="+profile, "--format", "process")
	if err != nil {
		return nil, err
	}
	var p struct {
		SessionToken string `json:"SessionToken"`
		Expiration   string `json:"Expiration"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("parsing export-credentials: %w", err)
	}
	if p.SessionToken != "" {
		return out, nil
	}
	sess, err := Runner(ctx, nil, nil, "aws", "sts", "get-session-token", "--profile="+profile, "--duration-seconds", "3600", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("profile %s has long-term keys and get-session-token failed (iris won't hand out long-term keys): %w", profile, err)
	}
	return sess, nil
}

// ---------------- gcp ----------------

// GCP prints an access token for your active gcloud account (target is the
// project the command runs against; the shim passes it to gcloud).
func GCP(ctx context.Context, _ string) ([]byte, error) {
	out, err := Runner(ctx, nil, nil, "gcloud", "auth", "print-access-token", "--verbosity=error")
	if err != nil {
		return nil, err
	}
	return emit(normalized{Token: strings.TrimSpace(string(out))})
}

// ---------------- git ----------------

// Git asks YOUR git credential helpers (osxkeychain, gh, manager...) with
// `git credential fill`, never prompting.
func Git(ctx context.Context, host string) ([]byte, error) {
	in := []byte("protocol=https\nhost=" + host + "\n\n")
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/usr/bin/false", "SSH_ASKPASS=/usr/bin/false", "GCM_INTERACTIVE=never"}
	out, err := Runner(ctx, env, in, "git", "credential", "fill")
	if err != nil {
		return nil, fmt.Errorf("no git credential for %s: %w", host, err)
	}
	var n normalized
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "username":
			n.Username = v
		case "password":
			n.Token = v
		case "password_expiry_utc":
			var sec int64
			if _, err := fmt.Sscan(v, &sec); err == nil {
				n.ExpiresAt = time.Unix(sec, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	if n.Token == "" {
		return nil, fmt.Errorf("your git credential helpers have nothing for %s", host)
	}
	n.LongLived = n.ExpiresAt == ""
	return emit(n)
}

// ---------------- docker ----------------

// Docker uses YOUR docker config: a per-registry helper, the credsStore, or
// an inline auth entry.
func Docker(ctx context.Context, registry string) ([]byte, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, ".docker")
	}
	var cfg struct {
		Auths       map[string]struct{ Auth string } `json:"auths"`
		CredsStore  string                           `json:"credsStore"`
		CredHelpers map[string]string                `json:"credHelpers"`
	}
	if b, err := ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	helper := cfg.CredHelpers[registry]
	if helper == "" {
		helper = cfg.CredsStore
	}
	if helper != "" {
		out, err := Runner(ctx, nil, []byte(registry), "docker-credential-"+helper, "get")
		if err != nil {
			return nil, fmt.Errorf("docker-credential-%s: %w", helper, err)
		}
		var r struct{ Username, Secret string }
		if err := json.Unmarshal(out, &r); err != nil {
			return nil, err
		}
		return emit(normalized{Token: r.Secret, Username: r.Username, LongLived: true})
	}
	for _, key := range []string{registry, "https://" + registry, "https://" + registry + "/v1/"} {
		if a, ok := cfg.Auths[key]; ok && a.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, err
			}
			u, p, _ := strings.Cut(string(dec), ":")
			return emit(normalized{Token: p, Username: u, LongLived: true})
		}
	}
	return nil, fmt.Errorf("no docker credential for %s in %s", registry, dir)
}
