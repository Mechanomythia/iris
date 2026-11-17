package passthrough

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/creds"
)

type call struct {
	env   []string
	stdin string
	argv  string
}

func fake(t *testing.T, resp map[string]string) *[]call {
	t.Helper()
	var calls []call
	old := Runner
	Runner = func(_ context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
		argv := name + " " + strings.Join(args, " ")
		calls = append(calls, call{env, string(stdin), argv})
		for prefix, r := range resp {
			if strings.HasPrefix(argv, prefix) {
				if r == "ERR" {
					return nil, errors.New("failed")
				}
				return []byte(r), nil
			}
		}
		return nil, errors.New("unexpected command: " + argv)
	}
	t.Cleanup(func() { Runner = old })
	return &calls
}

func parse(t *testing.T, tool string, raw []byte) *config.Identity {
	t.Helper()
	id := config.Identity{Tool: tool, Target: "x", Source: config.Source{Passthrough: true}}
	if _, err := creds.Parse(id, raw, time.Now()); err != nil {
		t.Fatalf("creds.Parse(%s): %v\n%s", tool, err, raw)
	}
	return &id
}

func TestKubeExecPlugin(t *testing.T) {
	calls := fake(t, map[string]string{
		"kubectl config view":    `{"users":[{"user":{"exec":{"apiVersion":"client.authentication.k8s.io/v1beta1","command":"gke-gcloud-auth-plugin","args":["--x"],"env":[{"name":"FOO","value":"bar"}]}}}]}`,
		"gke-gcloud-auth-plugin": `{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1beta1","status":{"token":"ya29.tok","expirationTimestamp":"2099-01-01T00:00:00Z"}}`,
	})
	raw, err := Kube(context.Background(), "gke_proj_europe-west4_kvasir")
	if err != nil {
		t.Fatal(err)
	}
	c, err := creds.Parse(config.Identity{Tool: "kube"}, raw, time.Now())
	if err != nil || c.Token != "ya29.tok" || c.LongLived {
		t.Fatalf("%v %+v", err, c)
	}
	if (*calls)[0].argv != "kubectl config view --raw --minify --flatten -o json --context=gke_proj_europe-west4_kvasir" {
		t.Fatalf("context must be passed as --context=, got %q", (*calls)[0].argv)
	}
	env := strings.Join((*calls)[1].env, " ")
	if !strings.Contains(env, "FOO=bar") || !strings.Contains(env, `KUBERNETES_EXEC_INFO={"apiVersion":"client.authentication.k8s.io/v1beta1"`) {
		t.Fatalf("exec env: %s", env)
	}
}

func TestKubeStaticAndCert(t *testing.T) {
	fake(t, map[string]string{"kubectl config view": `{"users":[{"user":{"token":"static"}}]}`})
	raw, _ := Kube(context.Background(), "c")
	c, err := creds.Parse(config.Identity{Tool: "kube"}, raw, time.Now())
	if err != nil || c.Token != "static" || !c.LongLived {
		t.Fatalf("%v %+v", err, c)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	fake(t, map[string]string{"kubectl config view": `{"users":[{"user":{"client-certificate-data":"` + b64("CERT") + `","client-key-data":"` + b64("KEY") + `"}}]}`})
	raw, _ = Kube(context.Background(), "c")
	c, err = creds.Parse(config.Identity{Tool: "kube"}, raw, time.Now())
	if err != nil || c.Fields["clientCertificateData"] != "CERT" || c.Fields["clientKeyData"] != "KEY" {
		t.Fatalf("%v %+v", err, c)
	}
	fake(t, map[string]string{"kubectl config view": `{"users":[{"user":{"auth-provider":{"name":"gcp"}}}]}`})
	if _, err := Kube(context.Background(), "c"); err == nil || !strings.Contains(err.Error(), "auth-provider") {
		t.Fatal(err)
	}
}

func TestAWS(t *testing.T) {
	// SSO / assume-role: already a session.
	fake(t, map[string]string{"aws configure export-credentials": `{"Version":1,"AccessKeyId":"ASIA","SecretAccessKey":"s","SessionToken":"t","Expiration":"2099-01-01T00:00:00Z"}`})
	raw, err := AWS(context.Background(), "prod-sso")
	if err != nil {
		t.Fatal(err)
	}
	parse(t, "aws", raw)

	// Long-term keys are swapped for a session token.
	calls := fake(t, map[string]string{
		"aws configure export-credentials": `{"Version":1,"AccessKeyId":"AKIA","SecretAccessKey":"longterm"}`,
		"aws sts get-session-token":        `{"Credentials":{"AccessKeyId":"ASIA2","SecretAccessKey":"s","SessionToken":"t","Expiration":"2099-01-01T00:00:00Z"}}`,
	})
	raw, err = AWS(context.Background(), "default")
	if err != nil || strings.Contains(string(raw), "longterm") {
		t.Fatalf("%v %s", err, raw)
	}
	if !strings.Contains((*calls)[1].argv, "--profile=default") {
		t.Fatal((*calls)[1].argv)
	}
	// ...and if that fails, the long-term key is NOT handed out.
	fake(t, map[string]string{
		"aws configure export-credentials": `{"Version":1,"AccessKeyId":"AKIA","SecretAccessKey":"longterm"}`,
		"aws sts get-session-token":        "ERR",
	})
	if raw, err := AWS(context.Background(), "default"); err == nil {
		t.Fatalf("long-term key leaked: %s", raw)
	}
}

func TestGitAndGCP(t *testing.T) {
	calls := fake(t, map[string]string{"git credential fill": "protocol=https\nhost=github.com\nusername=me\npassword=gho_x\n"})
	raw, err := Git(context.Background(), "github.com")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := creds.Parse(config.Identity{Tool: "git"}, raw, time.Now())
	if c.Token != "gho_x" || c.Username != "me" || !c.LongLived {
		t.Fatalf("%+v", c)
	}
	if (*calls)[0].stdin != "protocol=https\nhost=github.com\n\n" || !strings.Contains(strings.Join((*calls)[0].env, " "), "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("%+v", (*calls)[0])
	}
	fake(t, map[string]string{"gcloud auth print-access-token": "ya29.abc\n"})
	raw, _ = GCP(context.Background(), "proj")
	c, _ = creds.Parse(config.Identity{Tool: "gcp"}, raw, time.Now())
	if c.Token != "ya29.abc" {
		t.Fatalf("%+v", c)
	}
}

func TestDocker(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/dc")
	old := ReadFile
	t.Cleanup(func() { ReadFile = old })
	ReadFile = func(string) ([]byte, error) {
		return []byte(`{"credsStore":"osxkeychain","credHelpers":{"europe-docker.pkg.dev":"gcloud"},"auths":{"ghcr.io":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:p")) + `"}}}`), nil
	}
	calls := fake(t, map[string]string{"docker-credential-gcloud get": `{"Username":"oauth2accesstoken","Secret":"ya29"}`})
	raw, err := Docker(context.Background(), "europe-docker.pkg.dev")
	if err != nil || !strings.Contains(string(raw), "ya29") || (*calls)[0].stdin != "europe-docker.pkg.dev" {
		t.Fatalf("%v %s", err, raw)
	}
	ReadFile = func(string) ([]byte, error) {
		return []byte(`{"auths":{"ghcr.io":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:p")) + `"}}}`), nil
	}
	raw, err = Docker(context.Background(), "ghcr.io")
	if err != nil || !strings.Contains(string(raw), `"token":"p"`) {
		t.Fatalf("%v %s", err, raw)
	}
}

func TestTargetInjectionRejected(t *testing.T) {
	fake(t, nil)
	for _, bad := range []string{"--kubeconfig=/evil", "github.com\nhost=evil.com", "a b", "", "x;rm"} {
		if _, err := Fetch(context.Background(), "kube", bad); err == nil {
			t.Errorf("target %q accepted", bad)
		}
	}
}
