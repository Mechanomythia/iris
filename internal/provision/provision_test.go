package provision

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type call struct {
	stdin string
	argv  string
}

func fake(responses map[string]string) (*Runner, *[]call, *bytes.Buffer) {
	var calls []call
	out := &bytes.Buffer{}
	r := &Runner{Out: out, Exec: func(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		argv := name + " " + strings.Join(args, " ")
		calls = append(calls, call{string(stdin), argv})
		for prefix, resp := range responses {
			if strings.HasPrefix(argv, prefix) {
				if resp == "ERR" {
					return nil, errors.New("not found")
				}
				return []byte(resp), nil
			}
		}
		return nil, nil
	}}
	return r, &calls, out
}

func TestKube(t *testing.T) {
	r, calls, _ := fake(nil)
	id, err := Kube(context.Background(), r, KubeOptions{Context: "prod-admin", Name: "prod", ClusterRole: "view", BindNamespace: "app", TTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0].argv, "--context prod-admin apply -f -") {
		t.Fatalf("%v", *calls)
	}
	m := (*calls)[0].stdin
	for _, w := range []string{"kind: ServiceAccount", "name: claude-view", "kind: RoleBinding", "namespace: app", "automountServiceAccountToken: false"} {
		if !strings.Contains(m, w) {
			t.Errorf("manifest missing %q", w)
		}
	}
	if strings.Join(id.Source.Command, " ") != "kubectl --context prod-admin -n iris create token claude-view --duration 30m0s" {
		t.Fatalf("source %v", id.Source.Command)
	}
	if id.Target != "prod" || id.SourceContext != "prod-admin" || id.Namespace != "app" {
		t.Fatalf("%+v", id)
	}
}

func TestGCP(t *testing.T) {
	r, calls, _ := fake(map[string]string{"gcloud config get-value account": "me@example.com\n", "gcloud iam service-accounts describe": "ERR"})
	id, err := GCP(context.Background(), r, GCPOptions{Project: "p1", Roles: []string{"roles/viewer", "roles/logging.viewer"}})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, c := range *calls {
		all += c.argv + "\n"
	}
	for _, w := range []string{"service-accounts create claude-viewer --project p1", "--role roles/logging.viewer",
		"service-accounts add-iam-policy-binding claude-viewer@p1.iam.gserviceaccount.com", "--member user:me@example.com --role roles/iam.serviceAccountTokenCreator"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in\n%s", w, all)
		}
	}
	if !strings.Contains(strings.Join(id.Source.Command, " "), "--impersonate-service-account claude-viewer@p1.iam.gserviceaccount.com") {
		t.Fatal(id.Source.Command)
	}
}

func TestAWSDryRunAndPrincipals(t *testing.T) {
	r, calls, out := fake(map[string]string{
		"aws sts get-caller-identity": `{"Account":"123456789012","Arn":"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Admin_abc/me@example.com"}`,
		"aws iam get-role":            "ERR",
	})
	r.DryRun = true
	id, err := AWS(context.Background(), r, AWSOptions{Profile: "admin", Region: "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(c.argv, "create-role") || strings.Contains(c.argv, "attach-role-policy") {
			t.Fatal("dry-run executed a mutation: " + c.argv)
		}
	}
	if !strings.Contains(out.String(), "aws iam create-role --role-name iris-claude-read") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if !strings.Contains(out.String(), "role/*/AWSReservedSSO_Admin_abc") {
		t.Fatalf("SSO principal pattern missing:\n%s", out)
	}
	if id.Format != "aws-sts" || !strings.Contains(strings.Join(id.Source.Command, " "), "arn:aws:iam::123456789012:role/iris-claude-read") {
		t.Fatalf("%+v", id)
	}
}

func TestGitHubURL(t *testing.T) {
	u := GitHubTokenURL(GitHubOptions{Name: "claude", Host: "github.com", Owner: "acme", ExpiresDays: 7, Permissions: []string{"contents:write", "pull_requests"}})
	for _, w := range []string{"https://github.com/settings/personal-access-tokens/new?", "contents=write", "pull_requests=read", "target_name=acme", "expires_in=7"} {
		if !strings.Contains(u, w) {
			t.Errorf("url missing %q: %s", w, u)
		}
	}
}

func TestGitLab(t *testing.T) {
	r, calls, _ := fake(map[string]string{"glab config get token": "glpat-xyz\n"})
	id, err := GitLab(context.Background(), r, GitLabOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if id.Target != "gitlab.com" || id.ValueTemplate != "Bearer {{token}}" || id.RemoveHeaders[0] != "PRIVATE-TOKEN" ||
		id.SandboxEnv["GITLAB_TOKEN"] == "" || strings.Join(id.Source.Command, " ") != "glab config get token --host gitlab.com" {
		t.Fatalf("%+v", id)
	}
	if len(*calls) != 1 {
		t.Fatal(*calls)
	}
	r2, _, _ := fake(map[string]string{"glab config get token": ""})
	if _, err := GitLab(context.Background(), r2, GitLabOptions{Host: "git.acme.dev"}); err == nil || !strings.Contains(err.Error(), "glab auth login --hostname git.acme.dev") {
		t.Fatalf("expected a helpful error, got %v", err)
	}
}
