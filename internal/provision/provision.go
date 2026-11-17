// Package provision creates narrow, Claude-only identities using YOUR
// credentials (run from your own terminal, never from Claude), and returns
// the matching Iris identity config. The broker then mints short-lived
// credentials for these identities, never for your personal ones.
package provision

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/secrets"
)

// Runner executes (or, in dry-run, prints) provisioning commands.
type Runner struct {
	DryRun bool
	Out    io.Writer
	// Exec is swappable in tests.
	Exec func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

func NewRunner(dry bool, out io.Writer) *Runner {
	return &Runner{DryRun: dry, Out: out, Exec: func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}}
}

func (r *Runner) show(stdin []byte, name string, args ...string) {
	q := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \"'{}$*") {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			q[i] = a
		}
	}
	fmt.Fprintf(r.Out, "  $ %s %s\n", name, strings.Join(q, " "))
	if stdin != nil && r.DryRun {
		for _, l := range strings.Split(strings.TrimSpace(string(stdin)), "\n") {
			fmt.Fprintf(r.Out, "    | %s\n", l)
		}
	}
}

// Mutate runs a command that changes something (skipped in dry-run).
func (r *Runner) Mutate(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.show(stdin, name, args...)
	if r.DryRun {
		return nil, nil
	}
	return r.Exec(ctx, stdin, name, args...)
}

// Query runs a read-only command (also in dry-run, to resolve names).
func (r *Runner) Query(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.Exec(ctx, nil, name, args...)
}

// ---------------- Kubernetes ----------------

type KubeOptions struct {
	Context       string // your admin context
	Name          string // context name Claude sees (default: Context)
	ClusterRole   string // view | edit | admin | any ClusterRole (default view)
	Namespace     string // where the ServiceAccount lives (default iris)
	BindNamespace string // if set: RoleBinding limited to this namespace instead of cluster-wide
	TTL           time.Duration
}

func Kube(ctx context.Context, r *Runner, o KubeOptions) (config.Identity, error) {
	if o.Context == "" {
		return config.Identity{}, fmt.Errorf("--context is required")
	}
	def(&o.Name, o.Context)
	def(&o.ClusterRole, "view")
	def(&o.Namespace, "iris")
	if o.TTL == 0 {
		o.TTL = time.Hour
	}
	sa := "claude-" + sanitize(o.ClusterRole)
	binding := "iris-" + sa
	var doc strings.Builder
	fmt.Fprintf(&doc, `apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels: {app.kubernetes.io/managed-by: iris}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[2]s
  namespace: %[1]s
  labels: {app.kubernetes.io/managed-by: iris}
  annotations: {iris.dev/purpose: "Claude Code identity, tokens minted on approval"}
automountServiceAccountToken: false
---
`, o.Namespace, sa)
	if o.BindNamespace == "" {
		fmt.Fprintf(&doc, `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
  labels: {app.kubernetes.io/managed-by: iris}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %s}
subjects: [{kind: ServiceAccount, name: %s, namespace: %s}]
`, binding, o.ClusterRole, sa, o.Namespace)
	} else {
		fmt.Fprintf(&doc, `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s
  namespace: %s
  labels: {app.kubernetes.io/managed-by: iris}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: %s}
subjects: [{kind: ServiceAccount, name: %s, namespace: %s}]
`, binding, o.BindNamespace, o.ClusterRole, sa, o.Namespace)
	}
	if _, err := r.Mutate(ctx, []byte(doc.String()), "kubectl", "--context", o.Context, "apply", "-f", "-"); err != nil {
		return config.Identity{}, err
	}
	scope := o.ClusterRole
	if o.BindNamespace != "" {
		scope += " in " + o.BindNamespace
	}
	return config.Identity{
		Tool: config.ToolKube, Target: o.Name, SourceContext: o.Context, Scope: scope,
		Namespace: o.BindNamespace, MaxTTL: config.Duration{Duration: o.TTL},
		Source: config.Source{Command: []string{"kubectl", "--context", o.Context, "-n", o.Namespace,
			"create", "token", sa, "--duration", o.TTL.String()}},
		Note: "provisioned by iris: ServiceAccount " + o.Namespace + "/" + sa,
	}, nil
}

// ---------------- Google Cloud ----------------

type GCPOptions struct {
	Project string
	Name    string   // service account id (default claude-viewer)
	Roles   []string // default roles/viewer
	User    string   // who may impersonate (default: gcloud active account)
}

func GCP(ctx context.Context, r *Runner, o GCPOptions) (config.Identity, error) {
	if o.Project == "" {
		return config.Identity{}, fmt.Errorf("--project is required")
	}
	if len(o.Roles) == 0 {
		o.Roles = []string{"roles/viewer"}
	}
	def(&o.Name, "claude-"+sanitize(strings.TrimPrefix(o.Roles[0], "roles/")))
	if len(o.Name) > 30 {
		o.Name = o.Name[:30]
	}
	email := fmt.Sprintf("%s@%s.iam.gserviceaccount.com", o.Name, o.Project)
	if o.User == "" {
		out, err := r.Query(ctx, "gcloud", "config", "get-value", "account")
		if err != nil {
			return config.Identity{}, err
		}
		o.User = strings.TrimSpace(string(out))
		if o.User == "" {
			return config.Identity{}, fmt.Errorf("no active gcloud account; run gcloud auth login or pass --user")
		}
	}
	member := "user:" + o.User
	if strings.HasSuffix(o.User, ".gserviceaccount.com") {
		member = "serviceAccount:" + o.User
	}

	if _, err := r.Query(ctx, "gcloud", "iam", "service-accounts", "describe", email, "--project", o.Project, "--format=value(email)"); err != nil {
		if _, err := r.Mutate(ctx, nil, "gcloud", "iam", "service-accounts", "create", o.Name, "--project", o.Project,
			"--display-name", "Claude Code via Iris", "--description", "Tokens minted by Iris on approval"); err != nil {
			return config.Identity{}, err
		}
	} else {
		fmt.Fprintf(r.Out, "  service account %s already exists\n", email)
	}
	for _, role := range o.Roles {
		if _, err := r.Mutate(ctx, nil, "gcloud", "projects", "add-iam-policy-binding", o.Project,
			"--member", "serviceAccount:"+email, "--role", role, "--condition=None", "--quiet", "--format=none"); err != nil {
			return config.Identity{}, err
		}
	}
	if _, err := r.Mutate(ctx, nil, "gcloud", "iam", "service-accounts", "add-iam-policy-binding", email, "--project", o.Project,
		"--member", member, "--role", "roles/iam.serviceAccountTokenCreator", "--quiet", "--format=none"); err != nil {
		return config.Identity{}, err
	}
	return config.Identity{
		Tool: config.ToolGCP, Target: o.Project, Scope: strings.Join(o.Roles, ","),
		MaxTTL: config.Duration{Duration: time.Hour},
		Source: config.Source{Command: []string{"gcloud", "auth", "print-access-token", "--impersonate-service-account", email, "--verbosity=error"}},
		Note:   "provisioned by iris: " + email + " (impersonated by " + o.User + ")",
	}, nil
}

// ---------------- AWS ----------------

type AWSOptions struct {
	Profile  string // your admin profile used to create the role and to assume it later
	Name     string // Claude's profile name and role suffix (default claude-read)
	Policies []string
	Region   string
	Duration time.Duration
}

func AWS(ctx context.Context, r *Runner, o AWSOptions) (config.Identity, error) {
	def(&o.Name, "claude-read")
	if len(o.Policies) == 0 {
		o.Policies = []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}
	}
	if o.Duration == 0 {
		o.Duration = time.Hour
	}
	prof := []string{}
	if o.Profile != "" {
		prof = []string{"--profile", o.Profile}
	}
	out, err := r.Query(ctx, "aws", append([]string{"sts", "get-caller-identity", "--output", "json"}, prof...)...)
	if err != nil {
		return config.Identity{}, err
	}
	var who struct{ Account, Arn string }
	if err := json.Unmarshal(out, &who); err != nil {
		return config.Identity{}, fmt.Errorf("parsing get-caller-identity: %w", err)
	}
	principals := principalPatterns(who.Arn, who.Account)
	trust := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": "arn:aws:iam::" + who.Account + ":root"},
			"Action":    "sts:AssumeRole",
			"Condition": map[string]any{"ArnLike": map[string]any{"aws:PrincipalArn": principals}},
		}},
	}
	tb, _ := json.Marshal(trust)
	role := "iris-" + sanitize(o.Name)
	roleArn := "arn:aws:iam::" + who.Account + ":role/" + role
	secs := int(o.Duration.Seconds())
	maxSess := secs
	if maxSess < 3600 {
		maxSess = 3600
	}
	if _, err := r.Query(ctx, "aws", append([]string{"iam", "get-role", "--role-name", role, "--output", "json"}, prof...)...); err != nil {
		if _, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "create-role", "--role-name", role,
			"--assume-role-policy-document", string(tb), "--max-session-duration", fmt.Sprint(maxSess),
			"--description", "Claude Code via Iris", "--tags", "Key=managed-by,Value=iris"}, prof...)...); err != nil {
			return config.Identity{}, err
		}
	} else {
		fmt.Fprintf(r.Out, "  role %s exists, updating trust policy\n", role)
		if _, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "update-assume-role-policy", "--role-name", role,
			"--policy-document", string(tb)}, prof...)...); err != nil {
			return config.Identity{}, err
		}
	}
	for _, p := range o.Policies {
		if _, err := r.Mutate(ctx, nil, "aws", append([]string{"iam", "attach-role-policy", "--role-name", role, "--policy-arn", p}, prof...)...); err != nil {
			return config.Identity{}, err
		}
	}
	cmd := append([]string{"aws", "sts", "assume-role", "--role-arn", roleArn, "--role-session-name", "iris-claude",
		"--duration-seconds", fmt.Sprint(secs), "--output", "json"}, prof...)
	short := make([]string, len(o.Policies))
	for i, p := range o.Policies {
		short[i] = p[strings.LastIndex(p, "/")+1:]
	}
	fmt.Fprintln(r.Out, "  note: new IAM roles can take ~10s before they can be assumed.")
	return config.Identity{
		Tool: config.ToolAWS, Target: o.Name, Region: o.Region, Scope: strings.Join(short, ","),
		Format: "aws-sts", MaxTTL: config.Duration{Duration: o.Duration},
		Source: config.Source{Command: cmd},
		Note:   "provisioned by iris: " + roleArn,
	}, nil
}

// principalPatterns derives which of YOUR principals may assume the role.
// SSO / assumed-role sessions are matched by role name (any path).
func principalPatterns(arn, account string) []string {
	// arn:aws:sts::123:assumed-role/RoleName/session
	if i := strings.Index(arn, ":assumed-role/"); i >= 0 {
		rest := arn[i+len(":assumed-role/"):]
		role := rest
		if j := strings.Index(rest, "/"); j >= 0 {
			role = rest[:j]
		}
		return []string{"arn:aws:iam::" + account + ":role/" + role, "arn:aws:iam::" + account + ":role/*/" + role}
	}
	return []string{arn}
}

// ---------------- GitHub ----------------

type GitHubOptions struct {
	Name        string   // token name
	Owner       string   // resource owner (user or org) to pre-select
	Permissions []string // e.g. contents:read, pull_requests:write (metadata:read is implied)
	ExpiresDays int
	HTTP        bool // also add api.github.com for the proxy
	Host        string
	In          io.Reader // token input (default stdin)
	NoBrowser   bool
}

// GitHubTokenURL builds the pre-filled fine-grained PAT page. GitHub can't
// create PATs via API, so the user clicks "Generate" and pastes it back.
func GitHubTokenURL(o GitHubOptions) string {
	q := url.Values{}
	q.Set("name", o.Name)
	q.Set("description", "Claude Code via Iris. Brokered: only used after approval.")
	if o.Owner != "" {
		q.Set("target_name", o.Owner)
	}
	if o.ExpiresDays > 0 {
		q.Set("expires_in", fmt.Sprint(o.ExpiresDays))
	}
	for _, p := range o.Permissions {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			v = "read"
		}
		q.Set(k, v)
	}
	return "https://" + o.Host + "/settings/personal-access-tokens/new?" + q.Encode()
}

func GitHub(ctx context.Context, r *Runner, o GitHubOptions) ([]config.Identity, error) {
	def(&o.Name, "claude-iris-"+time.Now().Format("20060102"))
	def(&o.Host, "github.com")
	if len(o.Permissions) == 0 {
		o.Permissions = []string{"contents:read", "metadata:read"}
	}
	if o.ExpiresDays == 0 {
		o.ExpiresDays = 30
	}
	u := GitHubTokenURL(o)
	fmt.Fprintf(r.Out, "Create a fine-grained token (pre-filled; pick the repositories, then Generate):\n  %s\n", u)
	if !r.DryRun && !o.NoBrowser {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		_ = exec.Command(opener, u).Start()
	}
	ids := []config.Identity{{
		Tool: config.ToolGit, Target: o.Host, Username: "x-access-token", Scope: strings.Join(o.Permissions, ","),
		Source: config.Source{Keychain: &config.Keychain{Service: "iris", Account: o.Host}},
		Note:   fmt.Sprintf("provisioned by iris: fine-grained PAT %q, expires in %dd", o.Name, o.ExpiresDays),
	}}
	if o.HTTP {
		api := "api." + o.Host
		if o.Host != "github.com" {
			api = o.Host // GHES serves the API under /api/v3 on the same host
		}
		ids = append(ids, config.Identity{Tool: config.ToolHTTP, Target: api, Scope: ids[0].Scope,
			Source: ids[0].Source, Note: ids[0].Note})
	}
	if r.DryRun {
		fmt.Fprintf(r.Out, "  (dry-run) would store the pasted token in Keychain service=iris account=%s\n", o.Host)
		return ids, nil
	}
	tok, err := readSecret(o.In, r.Out, "Paste the token (input hidden): ")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(tok, "github_pat_") && !strings.HasPrefix(tok, "ghp_") {
		fmt.Fprintln(r.Out, "  warning: that does not look like a GitHub token; storing anyway")
	}
	if err := secrets.StoreKeychain(ctx, "iris", o.Host, tok); err != nil {
		return nil, err
	}
	fmt.Fprintf(r.Out, "  stored in Keychain (service=iris, account=%s)\n", o.Host)
	return ids, nil
}

// ---------------- generic secret ----------------

type SecretOptions struct {
	Tool, Target, Scope, Header, ValueTemplate, Username string
	In                                                   io.Reader
}

// Secret stores a pasted secret in the Keychain and returns an identity for it.
func Secret(ctx context.Context, r *Runner, o SecretOptions) (config.Identity, error) {
	if o.Tool == "" || o.Target == "" {
		return config.Identity{}, fmt.Errorf("--tool and --target are required")
	}
	account := o.Tool + "/" + o.Target
	id := config.Identity{Tool: o.Tool, Target: o.Target, Scope: o.Scope, Header: o.Header, ValueTemplate: o.ValueTemplate,
		Username: o.Username, Source: config.Source{Keychain: &config.Keychain{Service: "iris", Account: account}}}
	if r.DryRun {
		fmt.Fprintf(r.Out, "  (dry-run) would store the secret in Keychain service=iris account=%s\n", account)
		return id, nil
	}
	s, err := readSecret(o.In, r.Out, "Secret for "+account+" (input hidden): ")
	if err != nil {
		return config.Identity{}, err
	}
	return id, secrets.StoreKeychain(ctx, "iris", account, s)
}

// ---------------- helpers ----------------

func readSecret(in io.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if in == nil {
		in = os.Stdin
		if f, ok := in.(*os.File); ok && isTTY(f) {
			stty := func(arg string) {
				c := exec.Command("stty", arg)
				c.Stdin = f
				_ = c.Run()
			}
			stty("-echo")
			defer func() { stty("echo"); fmt.Fprintln(out) }()
		}
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return "", fmt.Errorf("empty secret")
	}
	return s, nil
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func def(s *string, v string) {
	if *s == "" {
		*s = v
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// ---------------- GitLab (glab, API) ----------------

type GitLabOptions struct {
	Host  string // gitlab.com or your instance
	Paste bool   // store a pasted token in the Keychain instead of reading glab's
	In    io.Reader
}

// GitLab lets glab (and curl to the API) work in the sandbox with YOUR
// token, which never enters it: glab gets a placeholder GITLAB_TOKEN, and
// the iris proxy replaces the PRIVATE-TOKEN header it sends with
// "Authorization: Bearer <your token>" (accepted for personal access and
// OAuth tokens). The token is read on your Mac, after approval, from glab's
// own store (glab config get token), or from the Keychain with --paste.
func GitLab(ctx context.Context, r *Runner, o GitLabOptions) (config.Identity, error) {
	def(&o.Host, "gitlab.com")
	id := config.Identity{
		Tool: config.ToolHTTP, Target: o.Host, Scope: "GitLab API (your token)",
		Header: "Authorization", ValueTemplate: "Bearer {{token}}",
		RemoveHeaders: []string{"PRIVATE-TOKEN", "JOB-TOKEN"},
		SandboxEnv: map[string]string{"GITLAB_TOKEN": "iris-injects-the-real-token", "GITLAB_HOST": o.Host,
			"GLAB_SEND_TELEMETRY": "false"},
		BlockPaths: []string{"/api/v4/usage_data/"}, // glab telemetry: no prompt, no token
		Note:       "provisioned by iris: glab in the sandbox uses a placeholder token; the proxy injects yours",
	}
	if o.Paste {
		id.Source = config.Source{Keychain: &config.Keychain{Service: "iris", Account: "http/" + o.Host}}
		if r.DryRun {
			fmt.Fprintf(r.Out, "  (dry-run) would store the pasted token in Keychain service=iris account=http/%s\n", o.Host)
			return id, nil
		}
		tok, err := readSecret(o.In, r.Out, "GitLab token for "+o.Host+" (input hidden): ")
		if err != nil {
			return id, err
		}
		return id, secrets.StoreKeychain(ctx, "iris", "http/"+o.Host, tok)
	}
	id.Source = config.Source{Command: []string{"glab", "config", "get", "token", "--host", o.Host}}
	if r.DryRun {
		fmt.Fprintf(r.Out, "  (dry-run) would read your token with: glab config get token --host %s\n", o.Host)
		return id, nil
	}
	out, err := r.Query(ctx, "glab", "config", "get", "token", "--host", o.Host)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return id, fmt.Errorf("glab has no token for %s here (run `glab auth login --hostname %s`, or use --paste with a personal access token): %v", o.Host, o.Host, err)
	}
	fmt.Fprintf(r.Out, "  glab has a token for %s; iris will read it after each approval\n", o.Host)
	return id, nil
}
