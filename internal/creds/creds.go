// Package creds turns raw source output into a protocol.Credential, and a
// Credential into the exact output each CLI expects from its helper.
package creds

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/protocol"
)

// DefaultUnknownExpiry is used when the source does not say when the secret
// expires (static PATs, raw tokens). The broker clamps it to the grant anyway.
const DefaultUnknownExpiry = time.Hour

// Parse interprets raw source output for an identity.
func Parse(id config.Identity, raw []byte, now time.Time) (*protocol.Credential, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, errors.New("source returned an empty secret")
	}
	format := id.Format
	if format == "" {
		format = detect(s)
	}
	c := &protocol.Credential{Kind: id.Tool, Fields: map[string]string{}}
	switch format {
	case "token":
		c.Token = s
	case "exec-credential":
		var ec struct {
			Status struct {
				Token                 string `json:"token"`
				ExpirationTimestamp   string `json:"expirationTimestamp"`
				ClientCertificateData string `json:"clientCertificateData"`
				ClientKeyData         string `json:"clientKeyData"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(s), &ec); err != nil {
			return nil, fmt.Errorf("exec-credential: %w", err)
		}
		c.Token = ec.Status.Token
		if ec.Status.ClientCertificateData != "" {
			c.Fields["clientCertificateData"] = ec.Status.ClientCertificateData
			c.Fields["clientKeyData"] = ec.Status.ClientKeyData
		}
		c.ExpiresAt = parseTime(ec.Status.ExpirationTimestamp)
	case "aws-process", "aws-sts":
		var p struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			SessionToken    string `json:"SessionToken"`
			Expiration      string `json:"Expiration"`
		}
		var wrap struct {
			Credentials *json.RawMessage `json:"Credentials"`
		}
		body := []byte(s)
		if json.Unmarshal(body, &wrap) == nil && wrap.Credentials != nil {
			body = *wrap.Credentials
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("aws credentials: %w", err)
		}
		if p.AccessKeyID == "" || p.SecretAccessKey == "" {
			return nil, errors.New("aws credentials: missing AccessKeyId/SecretAccessKey")
		}
		c.Fields["AccessKeyId"] = p.AccessKeyID
		c.Fields["SecretAccessKey"] = p.SecretAccessKey
		if p.SessionToken != "" {
			c.Fields["SessionToken"] = p.SessionToken
		}
		c.ExpiresAt = parseTime(p.Expiration)
	case "json":
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		for _, k := range []string{"token", "access_token", "accessToken", "password", "secret"} {
			if v, ok := m[k].(string); ok && v != "" {
				c.Token = v
				break
			}
		}
		if u, ok := m["username"].(string); ok {
			c.Username = u
		}
		for _, k := range []string{"expiresAt", "expires_at", "expiry", "expiration", "token_expiry"} {
			if v, ok := m[k].(string); ok {
				c.ExpiresAt = parseTime(v)
				break
			}
		}
		if v, ok := m["expires_in"].(float64); ok && c.ExpiresAt.IsZero() {
			c.ExpiresAt = now.Add(time.Duration(v) * time.Second)
		}
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
	if c.Token == "" && len(c.Fields) == 0 {
		return nil, errors.New("no usable secret in source output")
	}
	if id.Tool == config.ToolAWS && c.Fields["AccessKeyId"] == "" {
		return nil, errors.New("aws identity needs aws-process or aws-sts JSON from its source")
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = now.Add(DefaultUnknownExpiry)
	}
	if id.MaxTTL.Duration > 0 && c.ExpiresAt.After(now.Add(id.MaxTTL.Duration)) {
		c.ExpiresAt = now.Add(id.MaxTTL.Duration)
	}
	if c.Username == "" {
		c.Username = id.Username
	}
	return c, nil
}

func detect(s string) string {
	if !strings.HasPrefix(s, "{") {
		return "token"
	}
	switch {
	case strings.Contains(s, `"ExecCredential"`):
		return "exec-credential"
	case strings.Contains(s, `"AccessKeyId"`):
		return "aws-process"
	default:
		return "json"
	}
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, f := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---- helper-side rendering ----

// KubeExecCredential renders client.authentication.k8s.io/v1 ExecCredential.
func KubeExecCredential(c *protocol.Credential) ([]byte, error) {
	status := map[string]string{"expirationTimestamp": c.ExpiresAt.UTC().Format(time.RFC3339)}
	if c.Token != "" {
		status["token"] = c.Token
	}
	if v := c.Fields["clientCertificateData"]; v != "" {
		status["clientCertificateData"] = v
		status["clientKeyData"] = c.Fields["clientKeyData"]
	}
	return json.Marshal(map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1",
		"kind":       "ExecCredential",
		"status":     status,
	})
}

// AWSProcess renders the credential_process JSON.
func AWSProcess(c *protocol.Credential) ([]byte, error) {
	m := map[string]any{
		"Version":         1,
		"AccessKeyId":     c.Fields["AccessKeyId"],
		"SecretAccessKey": c.Fields["SecretAccessKey"],
		"Expiration":      c.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if v := c.Fields["SessionToken"]; v != "" {
		m["SessionToken"] = v
	}
	return json.Marshal(m)
}

// GitCredential renders git-credential "get" output.
func GitCredential(c *protocol.Credential, username string) []byte {
	u := c.Username
	if u == "" {
		u = username
	}
	if u == "" {
		u = "x-access-token"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "username=%s\npassword=%s\n", u, c.Token)
	if !c.ExpiresAt.IsZero() {
		// git >= 2.41 honours password_expiry_utc and won't cache past it.
		fmt.Fprintf(&b, "password_expiry_utc=%d\n", c.ExpiresAt.Unix())
	}
	return []byte(b.String())
}

// DockerCredential renders docker-credential-helper "get" output.
func DockerCredential(c *protocol.Credential, serverURL string) ([]byte, error) {
	u := c.Username
	if u == "" {
		u = "oauth2accesstoken"
	}
	return json.Marshal(map[string]string{"ServerURL": serverURL, "Username": u, "Secret": c.Token})
}

// HeaderValue renders the value injected by the proxy.
func HeaderValue(id config.Identity, c *protocol.Credential) string {
	tmpl := id.ValueTemplate
	if tmpl == "" {
		tmpl = "Bearer {{token}}"
	}
	v := strings.ReplaceAll(tmpl, "{{token}}", c.Token)
	if strings.Contains(v, "{{basic}}") {
		v = strings.ReplaceAll(v, "{{basic}}", base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Token)))
	}
	return v
}
