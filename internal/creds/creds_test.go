package creds

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mechanomythia/iris/internal/config"
)

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestParseToken(t *testing.T) {
	c, err := Parse(config.Identity{Tool: "kube", MaxTTL: config.Duration{Duration: 10 * time.Minute}}, []byte("abc\n"), now)
	if err != nil || c.Token != "abc" {
		t.Fatalf("%v %+v", err, c)
	}
	if !c.ExpiresAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("maxTTL not applied: %v", c.ExpiresAt)
	}
}

func TestParseExecCredential(t *testing.T) {
	raw := `{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1beta1","status":{"token":"t0k","expirationTimestamp":"2026-11-16T12:30:00Z"}}`
	c, err := Parse(config.Identity{Tool: "kube"}, []byte(raw), now)
	if err != nil || c.Token != "t0k" || c.ExpiresAt.Sub(now) != 30*time.Minute {
		t.Fatalf("%v %+v", err, c)
	}
	out, _ := KubeExecCredential(c)
	var back map[string]any
	_ = json.Unmarshal(out, &back)
	if back["apiVersion"] != "client.authentication.k8s.io/v1" || back["status"].(map[string]any)["token"] != "t0k" {
		t.Fatalf("bad render %s", out)
	}
}

func TestParseAWS(t *testing.T) {
	sts := `{"Credentials":{"AccessKeyId":"AKIA","SecretAccessKey":"s","SessionToken":"st","Expiration":"2026-11-16T13:00:00+00:00"},"AssumedRoleUser":{}}`
	c, err := Parse(config.Identity{Tool: "aws"}, []byte(sts), now)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := AWSProcess(c)
	if !strings.Contains(string(out), `"Version":1`) || !strings.Contains(string(out), `"SessionToken":"st"`) {
		t.Fatalf("%s", out)
	}
	if _, err := Parse(config.Identity{Tool: "aws"}, []byte("plain"), now); err == nil {
		t.Fatal("aws with plain token should fail")
	}
}

func TestGitAndHeader(t *testing.T) {
	c, _ := Parse(config.Identity{Tool: "git", Username: "bot"}, []byte("pat"), now)
	g := string(GitCredential(c, ""))
	if !strings.Contains(g, "username=bot\npassword=pat\n") || !strings.Contains(g, "password_expiry_utc=") {
		t.Fatal(g)
	}
	if v := HeaderValue(config.Identity{}, c); v != "Bearer pat" {
		t.Fatal(v)
	}
	if v := HeaderValue(config.Identity{ValueTemplate: "Basic {{basic}}"}, c); v != "Basic Ym90OnBhdA==" {
		t.Fatal(v)
	}
}
