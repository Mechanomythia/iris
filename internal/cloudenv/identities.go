package cloudenv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mechanomythia/iris/internal/provision"
)

// Identities are long-lived on purpose: a cloud session can't reach your Mac
// to mint short-lived credentials. So each environment gets its own narrow
// identity (service account / IAM user) that you can revoke on its own.

func saID(env string) string {
	id := "claude-" + sanitize(env)
	if len(id) > 30 {
		id = strings.TrimRight(id[:30], "-")
	}
	if len(id) < 6 {
		id += "-env"
	}
	return id
}

// CreateGCP makes one service account (in the first project) with every role
// the request needs, across projects, and returns a base64 key.
func CreateGCP(ctx context.Context, r *provision.Runner, req Request) (email, keyB64 string, err error) {
	projects := req.GCPProjects()
	if len(projects) == 0 {
		return "", "", nil
	}
	home := req.PrimaryProject()
	id := saID(req.Name)
	email = fmt.Sprintf("%s@%s.iam.gserviceaccount.com", id, home)
	if _, err := r.Query(ctx, "gcloud", "iam", "service-accounts", "describe", email, "--project", home, "--format=value(email)"); err != nil {
		if _, err := r.Mutate(ctx, nil, "gcloud", "iam", "service-accounts", "create", id, "--project", home,
			"--display-name", "Claude Code cloud env "+req.Name, "--description", "Created by iris cloud new; key lives in the Claude environment"); err != nil {
			return "", "", err
		}
	} else {
		fmt.Fprintf(r.Out, "  service account %s exists; adding roles and a new key\n", email)
	}
	names := make([]string, 0, len(projects))
	for p := range projects {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		for _, role := range projects[p] {
			if _, err := r.Mutate(ctx, nil, "gcloud", "projects", "add-iam-policy-binding", p,
				"--member", "serviceAccount:"+email, "--role", role, "--condition=None", "--quiet", "--format=none"); err != nil {
				return "", "", err
			}
		}
	}
	dir, err := os.MkdirTemp("", "iris-key-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	keyFile := filepath.Join(dir, "key.json")
	if _, err := r.Mutate(ctx, nil, "gcloud", "iam", "service-accounts", "keys", "create", keyFile, "--iam-account", email, "--project", home, "--quiet"); err != nil {
		if strings.Contains(err.Error(), "disableServiceAccountKeyCreation") {
			return "", "", fmt.Errorf("your organization blocks service-account keys (constraints/iam.disableServiceAccountKeyCreation); an org admin must allow it for %s: %w", home, err)
		}
		return "", "", err
	}
	if r.DryRun {
		return email, "", nil
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return "", "", err
	}
	return email, base64.StdEncoding.EncodeToString(b), nil
}

// CreateAWS makes an IAM user with the policies and an access key.
func CreateAWS(ctx context.Context, r *provision.Runner, req Request, profile string) (user, keyID, secret string, err error) {
	if req.AWS == nil {
		return "", "", "", nil
	}
	user = "iris-claude-" + sanitize(req.Name)
	prof := []string{}
	if profile != "" {
		prof = []string{"--profile", profile}
	}
	if _, err := r.Query(ctx, "aws", append([]string{"iam", "get-user", "--user-name", user, "--output", "json"}, prof...)...); err != nil {
		if _, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "create-user", "--user-name", user,
			"--tags", "Key=managed-by,Value=iris", "Key=claude-env,Value=" + sanitize(req.Name)}, prof...)...); err != nil {
			return "", "", "", err
		}
	} else {
		fmt.Fprintf(r.Out, "  IAM user %s exists; adding policies and a new access key (an IAM user holds at most 2)\n", user)
	}
	pols := req.AWS.Policies
	if len(pols) == 0 {
		pols = AWSPolicies(req.AWS.Access)
	}
	for _, p := range pols {
		if _, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "attach-user-policy", "--user-name", user, "--policy-arn", p}, prof...)...); err != nil {
			return "", "", "", err
		}
	}
	out, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "create-access-key", "--user-name", user, "--output", "json"}, prof...)...)
	if err != nil || r.DryRun {
		return user, "", "", err
	}
	var resp struct {
		AccessKey struct{ AccessKeyId, SecretAccessKey string }
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", "", "", fmt.Errorf("parsing create-access-key: %w", err)
	}
	return user, resp.AccessKey.AccessKeyId, resp.AccessKey.SecretAccessKey, nil
}

// DockerTokenURL is the page to create a registry token with the right scope.
func DockerTokenURL(d Docker, env string) string {
	if d.Registry == "ghcr.io" {
		scopes := "read:packages"
		if d.Access == Write {
			scopes = "write:packages"
		}
		q := url.Values{"scopes": {scopes}, "description": {"Claude cloud env " + env}}
		return "https://github.com/settings/tokens/new?" + q.Encode()
	}
	return "https://app.docker.com/settings/personal-access-tokens" // pick Read-only or Read & Write there
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}
