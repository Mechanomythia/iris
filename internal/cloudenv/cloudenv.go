// Package cloudenv plans a Claude Code cloud environment (claude.ai/code):
// network access, allowed domains, environment variables, network secrets
// and a setup script, from a short list of "Claude needs X with read/write
// access". There is no API to create environments, so the output is a guide
// the user pastes into the environment dialog (iris walks them through it).
//
// Facts this relies on (code.claude.com/docs/en/cloud-environments):
//   - Network secrets: the agent proxy adds a header per host after the
//     request leaves the VM; the key never enters the VM. Static headers only,
//     so they fit API tokens, not AWS SigV4 or GCP's key-for-token exchange.
//     GitHub, package registries and setup-script requests never get them.
//   - Environment variables: plain .env lines, readable by every session and
//     every person using the environment.
//   - Setup script: bash, as root on Ubuntu 24.04, <= 64 KB, ~5 min, must exit
//     0, runs BEFORE the environment's variables and secrets exist.
//   - GitHub: handled by the GitHub proxy for attached repositories.
//   - Default allowlist (Limited + package managers) already covers
//     *.googleapis.com, *.amazonaws.com, Docker Hub, gcr.io, ghcr.io.
package cloudenv

import (
	"fmt"
	"sort"
	"strings"
)

type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Request is what the user wants Claude to reach.
type Request struct {
	Name     string
	GitHub   bool
	GCP      *GCP
	Firebase *Firebase
	GKE      *GKE
	Registry *ArtifactRegistry
	AWS      *AWS
	Docker   *Docker
	APIs     []API
}

type GCP struct {
	Project string
	Access  Access
	Roles   []string // override; empty = defaults for Access
}

type Firebase struct {
	Project string
	Access  Access
}

type GKE struct {
	Project, Cluster, Location string
	Access                     Access
}

type ArtifactRegistry struct {
	Project, Location string // location e.g. europe-west4
	Access            Access
}

type AWS struct {
	Region   string
	Access   Access
	Policies []string // override
}

type Docker struct {
	Registry string // docker.io | ghcr.io
	Username string
	Access   Access
}

// API is an HTTP API reached with a token, served as a network secret.
type API struct {
	Name   string
	Host   string
	Header string
	Prefix string // e.g. "Bearer"; empty = bare value
}

// APIPresets are common token APIs. api.anthropic.com is deliberately absent:
// the proxy never attaches secrets to it.
var APIPresets = []API{
	{Name: "OpenAI", Host: "api.openai.com", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Stripe", Host: "api.stripe.com", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Cloudflare", Host: "api.cloudflare.com", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Sentry", Host: "sentry.io", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Linear", Host: "api.linear.app", Header: "Authorization", Prefix: ""},
	{Name: "Vercel", Host: "api.vercel.com", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Hugging Face", Host: "huggingface.co", Header: "Authorization", Prefix: "Bearer"},
	{Name: "Slack", Host: "slack.com", Header: "Authorization", Prefix: "Bearer"},
}

// Default roles / policies per access level.
func GCPRoles(a Access) []string {
	if a == Write {
		return []string{"roles/editor"}
	}
	return []string{"roles/viewer"}
}

func FirebaseRoles(a Access) []string {
	if a == Write {
		return []string{"roles/firebase.admin"}
	}
	return []string{"roles/firebase.viewer"}
}

func GKERoles(a Access) []string {
	if a == Write {
		return []string{"roles/container.developer"}
	}
	return []string{"roles/container.viewer"}
}

func RegistryRoles(a Access) []string {
	if a == Write {
		return []string{"roles/artifactregistry.writer"}
	}
	return []string{"roles/artifactregistry.reader"}
}

func AWSPolicies(a Access) []string {
	if a == Write {
		return []string{"arn:aws:iam::aws:policy/PowerUserAccess"}
	}
	return []string{"arn:aws:iam::aws:policy/ReadOnlyAccess"}
}

// GCPProjects groups every Google role the request needs by project, so one
// service account per project covers GCP, Firebase, GKE and Artifact Registry.
func (r Request) GCPProjects() map[string][]string {
	m := map[string][]string{}
	add := func(p string, roles ...string) {
		if p == "" {
			return
		}
		for _, role := range roles {
			if !contains(m[p], role) {
				m[p] = append(m[p], role)
			}
		}
	}
	if r.GCP != nil {
		roles := r.GCP.Roles
		if len(roles) == 0 {
			roles = GCPRoles(r.GCP.Access)
		}
		add(r.GCP.Project, roles...)
	}
	if r.Firebase != nil {
		add(r.Firebase.Project, FirebaseRoles(r.Firebase.Access)...)
	}
	if r.GKE != nil {
		add(r.GKE.Project, GKERoles(r.GKE.Access)...)
	}
	if r.Registry != nil {
		add(r.Registry.Project, RegistryRoles(r.Registry.Access)...)
	}
	return m
}

// PrimaryProject is the project the gcloud CLI defaults to.
func (r Request) PrimaryProject() string {
	for _, p := range []string{pick(r.GCP != nil, func() string { return r.GCP.Project }),
		pick(r.Firebase != nil, func() string { return r.Firebase.Project }),
		pick(r.GKE != nil, func() string { return r.GKE.Project }),
		pick(r.Registry != nil, func() string { return r.Registry.Project })} {
		if p != "" {
			return p
		}
	}
	return ""
}

func (r Request) needsGoogle() bool { return len(r.GCPProjects()) > 0 }

// ---------------- the guide ----------------

type EnvVar struct {
	Key, Value string
	Secret     bool // a credential: readable by everyone using the environment
}

type NetSecret struct {
	Service string
	Host    string
	Header  string
	Prefix  string
	Value   string // where to get it
}

type Guide struct {
	Name           string
	Network        string // Limited
	PackageManager bool
	AllowedDomains []string
	EnvVars        []EnvVar
	NetSecrets     []NetSecret
	SetupScript    string
	Repositories   bool
	Warnings       []string
	FirstSession   string // prompt to paste into the first session
}

// Credentials are values filled in after identities are created.
type Credentials struct {
	GCPKeyB64     string // base64 service-account key JSON (primary project)
	AWSKeyID      string
	AWSSecret     string
	DockerToken   string
	GCPServiceAcc string // email, informational
	AWSUser       string
}

const (
	KeyPath   = "/root/.config/iris/gcp-sa.json"
	EnvScript = "/etc/iris-cloud-env.sh"
)

func placeholder(what string) string { return "<" + what + ">" }

// Build turns a request (and any credentials already created) into a guide.
func Build(r Request, c Credentials) Guide {
	g := Guide{Name: r.Name, Network: "Limited", PackageManager: true, Repositories: r.GitHub}
	domains := map[string]bool{}
	val := func(v, what string) string {
		if v != "" {
			return v
		}
		return placeholder(what)
	}

	if r.needsGoogle() {
		domains["packages.cloud.google.com"] = true // apt repo for google-cloud-cli
		g.EnvVars = append(g.EnvVars,
			EnvVar{Key: "IRIS_GCP_SA_KEY_B64", Value: val(c.GCPKeyB64, "base64 service-account key, from iris"), Secret: true},
			EnvVar{Key: "GOOGLE_APPLICATION_CREDENTIALS", Value: KeyPath},
			EnvVar{Key: "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", Value: KeyPath},
			EnvVar{Key: "CLOUDSDK_CORE_DISABLE_PROMPTS", Value: "1"},
		)
		if p := r.PrimaryProject(); p != "" {
			g.EnvVars = append(g.EnvVars, EnvVar{Key: "CLOUDSDK_CORE_PROJECT", Value: p}, EnvVar{Key: "GOOGLE_CLOUD_PROJECT", Value: p})
		}
		if len(r.GCPProjects()) > 1 {
			g.Warnings = append(g.Warnings, "Several GCP projects: iris creates the service account in the first and grants roles in the others.")
		}
	}
	if r.Firebase != nil {
		domains["*.firebaseio.com"] = true
		domains["firebase.google.com"] = true
	}
	if r.GKE != nil {
		domains["*.gke.goog"] = true // GKE DNS-based control-plane endpoint
		g.EnvVars = append(g.EnvVars,
			EnvVar{Key: "IRIS_GKE_CLUSTER", Value: r.GKE.Cluster},
			EnvVar{Key: "IRIS_GKE_LOCATION", Value: r.GKE.Location},
			EnvVar{Key: "USE_GKE_GCLOUD_AUTH_PLUGIN", Value: "True"})
		g.Warnings = append(g.Warnings, "GKE: the cluster must have its DNS-based endpoint enabled (gcloud container clusters update "+
			r.GKE.Cluster+" --location "+r.GKE.Location+" --enable-dns-access); the IP endpoint isn't reachable under Limited access.")
	}
	if r.Registry != nil {
		domains["*.pkg.dev"] = true
		g.EnvVars = append(g.EnvVars, EnvVar{Key: "IRIS_GAR_HOST", Value: r.Registry.Location + "-docker.pkg.dev"})
	}
	if r.AWS != nil {
		g.EnvVars = append(g.EnvVars,
			EnvVar{Key: "AWS_ACCESS_KEY_ID", Value: val(c.AWSKeyID, "access key id, from iris"), Secret: true},
			EnvVar{Key: "AWS_SECRET_ACCESS_KEY", Value: val(c.AWSSecret, "secret access key, from iris"), Secret: true},
			EnvVar{Key: "AWS_DEFAULT_REGION", Value: r.AWS.Region},
			EnvVar{Key: "AWS_REGION", Value: r.AWS.Region})
	}
	if r.Docker != nil {
		reg := r.Docker.Registry
		if reg == "" {
			reg = "docker.io"
		}
		g.EnvVars = append(g.EnvVars,
			EnvVar{Key: "IRIS_DOCKER_REGISTRY", Value: reg},
			EnvVar{Key: "IRIS_DOCKER_USER", Value: r.Docker.Username},
			EnvVar{Key: "IRIS_DOCKER_TOKEN", Value: val(c.DockerToken, "registry token"), Secret: true})
	}
	for _, a := range r.APIs {
		prefix := a.Prefix
		g.NetSecrets = append(g.NetSecrets, NetSecret{Service: a.Name, Host: a.Host, Header: a.Header, Prefix: prefix,
			Value: "your " + a.Name + " token (paste it in the dialog; iris never sees it)"})
	}
	if bootstrapNeeded(r) {
		g.EnvVars = append(g.EnvVars, EnvVar{Key: "BASH_ENV", Value: EnvScript})
	}
	if hasSecretVars(g.EnvVars) {
		g.Warnings = append(g.Warnings, "Environment variables are readable by everyone who uses this environment, and by every command Claude runs. Keep it a personal environment; to rotate, run `iris cloud new` again with the same name (it adds a fresh key) and delete the old key.")
	}
	for d := range domains {
		g.AllowedDomains = append(g.AllowedDomains, d)
	}
	sort.Strings(g.AllowedDomains)
	g.SetupScript = SetupScript(r)
	g.FirstSession = firstSession(r)
	return g
}

func bootstrapNeeded(r Request) bool { return r.needsGoogle() || r.Docker != nil || r.GKE != nil }

func hasSecretVars(vs []EnvVar) bool {
	for _, v := range vs {
		if v.Secret {
			return true
		}
	}
	return false
}

// EnvBlock renders the variables in .env format for the dialog.
func (g Guide) EnvBlock() string {
	var b strings.Builder
	for _, v := range g.EnvVars {
		fmt.Fprintf(&b, "%s=%s\n", v.Key, v.Value)
	}
	return b.String()
}

// RedactedEnvBlock is EnvBlock with secret values replaced (for files and screens).
func (g Guide) RedactedEnvBlock() string {
	var b strings.Builder
	for _, v := range g.EnvVars {
		val := v.Value
		if v.Secret && !strings.HasPrefix(val, "<") {
			val = "<secret, in secrets.json>"
		}
		fmt.Fprintf(&b, "%s=%s\n", v.Key, val)
	}
	return b.String()
}

func firstSession(r Request) string {
	var checks []string
	if r.needsGoogle() {
		checks = append(checks, "gcloud auth list")
	}
	if r.GKE != nil {
		checks = append(checks, "kubectl get namespaces")
	}
	if r.AWS != nil {
		checks = append(checks, "aws sts get-caller-identity")
	}
	if r.Docker != nil {
		checks = append(checks, "docker info (start dockerd first if needed) and check the registry login")
	}
	for _, a := range r.APIs {
		checks = append(checks, fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' https://%s/ (expect the API to answer as authenticated)", a.Host))
	}
	if len(checks) == 0 {
		return "Run `git status` and `gh api repos/{owner}/{repo}` to confirm GitHub access."
	}
	return "Run `iris-cloud-auth --check`, then: " + strings.Join(checks, "; ") + ". Report what works and what fails, without printing any credential values."
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func pick(ok bool, f func() string) string {
	if !ok {
		return ""
	}
	return f()
}
