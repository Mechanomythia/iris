package cloudenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func full() Request {
	return Request{Name: "acme-api", GitHub: true,
		GCP:      &GCP{Project: "acme-prod", Access: Read},
		Firebase: &Firebase{Project: "acme-prod", Access: Write},
		GKE:      &GKE{Project: "acme-prod", Cluster: "kvasir", Location: "europe-west4", Access: Read},
		Registry: &ArtifactRegistry{Project: "acme-build", Location: "europe-west4", Access: Write},
		AWS:      &AWS{Region: "eu-west-1", Access: Read},
		Docker:   &Docker{Registry: "docker.io", Username: "acme", Access: Read},
		APIs:     []API{APIPresets[0], APIPresets[4]},
	}
}

func TestRolesGroupedPerProject(t *testing.T) {
	m := full().GCPProjects()
	if strings.Join(m["acme-prod"], ",") != "roles/viewer,roles/firebase.admin,roles/container.viewer" {
		t.Fatal(m["acme-prod"])
	}
	if strings.Join(m["acme-build"], ",") != "roles/artifactregistry.writer" {
		t.Fatal(m["acme-build"])
	}
	if full().PrimaryProject() != "acme-prod" {
		t.Fatal("primary project")
	}
}

func TestBuildGuide(t *testing.T) {
	g := Build(full(), Credentials{GCPKeyB64: "S0VZ", AWSKeyID: "AKIA1", AWSSecret: "s3cr3t", DockerToken: "dckr"})
	env := g.EnvBlock()
	for _, w := range []string{"IRIS_GCP_SA_KEY_B64=S0VZ", "AWS_SECRET_ACCESS_KEY=s3cr3t", "CLOUDSDK_CORE_PROJECT=acme-prod",
		"BASH_ENV=/etc/iris-cloud-env.sh", "IRIS_GKE_CLUSTER=kvasir", "IRIS_GAR_HOST=europe-west4-docker.pkg.dev", "IRIS_DOCKER_TOKEN=dckr"} {
		if !strings.Contains(env, w) {
			t.Errorf("env missing %s", w)
		}
	}
	red := g.RedactedEnvBlock()
	if strings.Contains(red, "s3cr3t") || strings.Contains(red, "S0VZ") || strings.Contains(red, "dckr") {
		t.Fatal("secrets in redacted block")
	}
	md := g.Markdown()
	if strings.Contains(md, "s3cr3t") || strings.Contains(md, "S0VZ") {
		t.Fatal("secrets in guide.md")
	}
	for _, d := range []string{"packages.cloud.google.com", "*.gke.goog", "*.pkg.dev", "*.firebaseio.com"} {
		if !strings.Contains(strings.Join(g.AllowedDomains, " "), d) {
			t.Errorf("domain %s missing", d)
		}
	}
	if len(g.NetSecrets) != 2 || g.NetSecrets[1].Host != "api.linear.app" || g.NetSecrets[1].Prefix != "" {
		t.Fatalf("%+v", g.NetSecrets)
	}
	if !strings.Contains(md, "| Linear | `api.linear.app` | `Authorization` | (clear it) |") {
		t.Fatal(md)
	}
	if !strings.Contains(strings.Join(g.Warnings, " "), "readable by everyone") || !strings.Contains(strings.Join(g.Warnings, " "), "--enable-dns-access") {
		t.Fatal(g.Warnings)
	}
}

func TestMinimalGitHubOnly(t *testing.T) {
	g := Build(Request{Name: "docs", GitHub: true}, Credentials{})
	if len(g.EnvVars) != 0 || len(g.AllowedDomains) != 0 || len(g.Warnings) != 0 {
		t.Fatalf("%+v", g)
	}
	if strings.Contains(g.SetupScript, "IRIS_ENV") {
		t.Fatal("no bootstrap needed for GitHub only")
	}
}

func TestPlaceholdersWithoutCredentials(t *testing.T) {
	g := Build(Request{Name: "x", AWS: &AWS{Region: "us-east-1", Access: Write}}, Credentials{})
	if !strings.Contains(g.EnvBlock(), "AWS_ACCESS_KEY_ID=<access key id, from iris>") {
		t.Fatal(g.EnvBlock())
	}
}

func TestSetupScriptsAreValidBash(t *testing.T) {
	for name, r := range map[string]Request{
		"full": full(), "aws": {Name: "a", AWS: &AWS{Region: "eu-west-1"}}, "github": {Name: "g", GitHub: true},
		"gcp": {Name: "g", GCP: &GCP{Project: "p"}}, "docker": {Name: "d", Docker: &Docker{Username: "u"}},
	} {
		f := filepath.Join(t.TempDir(), name+".sh")
		os.WriteFile(f, []byte(SetupScript(r)), 0o755)
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
		if len(SetupScript(r)) > 64*1024 {
			t.Errorf("%s: setup script over the 64 KB limit", name)
		}
	}
}
