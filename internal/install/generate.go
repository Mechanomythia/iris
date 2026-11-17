package install

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
)

// KubeClusterLookup returns the cluster entry (server, CA) for a context in
// the user's real kubeconfig. Swappable in tests.
var KubeClusterLookup = func(context string) (map[string]any, error) {
	out, err := exec.Command("kubectl", "config", "view", "--raw", "--minify", "--flatten", "-o", "json", "--context", context).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("kubectl config view --context %s: %s", context, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	var kc struct {
		Clusters []struct {
			Cluster map[string]any `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		return nil, err
	}
	if len(kc.Clusters) == 0 {
		return nil, fmt.Errorf("context %s has no cluster", context)
	}
	return kc.Clusters[0].Cluster, nil
}

// KubeContext is one context of the user's real kubeconfig.
type KubeContext struct {
	Name      string
	Namespace string
	Cluster   map[string]any // server, CA: not secret
}

// KubeContexts lists your contexts with their clusters. Swappable in tests.
var KubeContexts = func() ([]KubeContext, string, error) {
	out, err := exec.Command("kubectl", "config", "view", "--raw", "--flatten", "-o", "json").Output()
	if err != nil {
		return nil, "", fmt.Errorf("kubectl config view: %w", err)
	}
	var kc struct {
		Current  string `json:"current-context"`
		Clusters []struct {
			Name    string         `json:"name"`
			Cluster map[string]any `json:"cluster"`
		} `json:"clusters"`
		Contexts []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster   string `json:"cluster"`
				Namespace string `json:"namespace"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(out, &kc); err != nil {
		return nil, "", err
	}
	clusters := map[string]map[string]any{}
	for _, c := range kc.Clusters {
		clusters[c.Name] = c.Cluster
	}
	var res []KubeContext
	for _, c := range kc.Contexts {
		if cl, ok := clusters[c.Context.Cluster]; ok {
			res = append(res, KubeContext{Name: c.Name, Namespace: c.Context.Namespace, Cluster: cl})
		}
	}
	return res, kc.Current, nil
}

// AWSProfile is one profile of the user's real AWS config.
type AWSProfile struct{ Name, Region string }

// AWSProfiles lists your profiles. Swappable in tests.
var AWSProfiles = func() ([]AWSProfile, error) {
	out, err := exec.Command("aws", "configure", "list-profiles").Output()
	if err != nil {
		return nil, fmt.Errorf("aws configure list-profiles: %w", err)
	}
	var res []AWSProfile
	for _, name := range strings.Fields(string(out)) {
		r, _ := exec.Command("aws", "configure", "get", "region", "--profile="+name).Output()
		res = append(res, AWSProfile{Name: name, Region: strings.TrimSpace(string(r))})
	}
	return res, nil
}

// GcloudProject is your active gcloud project. Swappable in tests.
var GcloudProject = func() string {
	out, err := exec.Command("gcloud", "config", "get-value", "project").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Generate writes every Claude-facing file. None contain secrets.
func Generate(cfg *config.Config, o Options) ([]string, error) {
	var written, warnings []string
	write := func(p string, b []byte, mode os.FileMode) error {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			return err
		}
		written = append(written, p)
		return nil
	}

	// kubeconfig: passthrough mirrors every context you have (same names);
	// explicit identities add or override entries.
	if cfg.Uses(config.ToolKube) {
		type named struct {
			Name    string         `yaml:"name"`
			Cluster map[string]any `yaml:"cluster,omitempty"`
			User    map[string]any `yaml:"user,omitempty"`
			Context map[string]any `yaml:"context,omitempty"`
		}
		type entry struct {
			name, namespace string
			cluster         map[string]any
		}
		var order []string
		entries := map[string]entry{}
		put := func(e entry) {
			if _, ok := entries[e.name]; !ok {
				order = append(order, e.name)
			}
			entries[e.name] = e
		}
		cur := ""
		if cfg.PassthroughEnabled(config.ToolKube) {
			ctxs, current, err := KubeContexts()
			if err != nil {
				warnings = append(warnings, "kube passthrough: "+err.Error())
			}
			for _, kc := range ctxs {
				if config.ValidateTarget(kc.Name) != nil {
					warnings = append(warnings, fmt.Sprintf("kube context %q skipped: name has characters iris won't pass to kubectl", kc.Name))
					continue
				}
				put(entry{name: kc.Name, namespace: kc.Namespace, cluster: kc.Cluster})
			}
			cur = current
		}
		for _, id := range cfg.ByTool(config.ToolKube) {
			src := id.SourceContext
			if src == "" {
				src = id.Target
			}
			cl, err := KubeClusterLookup(src)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("kube %s: %v (context skipped)", id.Target, err))
				continue
			}
			put(entry{name: id.Target, namespace: id.Namespace, cluster: cl})
		}
		var clusters, users, contexts []named
		for _, n := range order {
			e := entries[n]
			ref := "iris-" + n
			clusters = append(clusters, named{Name: ref, Cluster: e.cluster})
			users = append(users, named{Name: ref, User: map[string]any{"exec": map[string]any{
				"apiVersion":         "client.authentication.k8s.io/v1",
				"command":            o.HelperBin,
				"args":               []string{"kube", n},
				"interactiveMode":    "Never",
				"provideClusterInfo": false,
			}}})
			ctx := map[string]any{"cluster": ref, "user": ref}
			if e.namespace != "" {
				ctx["namespace"] = e.namespace
			}
			contexts = append(contexts, named{Name: n, Context: ctx})
		}
		if _, ok := entries[cur]; !ok {
			cur = ""
			if len(order) > 0 {
				cur = order[0]
			}
		}
		kc := map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": clusters, "users": users,
			"contexts": contexts, "current-context": cur, "preferences": map[string]any{}}
		b, _ := yaml.Marshal(kc)
		if err := write(paths.ClaudeKubeconfig(), append([]byte("# Generated by `iris install`. Claude-only kubeconfig: no secrets, auth via iris-helper.\n"), b...), 0o644); err != nil {
			return written, err
		}
	}

	// AWS config: passthrough mirrors your profiles; explicit identities add/override.
	if cfg.Uses(config.ToolAWS) {
		regions := map[string]string{}
		var order []string
		add := func(name, region string) {
			if _, ok := regions[name]; !ok {
				order = append(order, name)
			}
			if region != "" || regions[name] == "" {
				regions[name] = region
			}
		}
		if cfg.PassthroughEnabled(config.ToolAWS) {
			profiles, err := AWSProfiles()
			if err != nil {
				warnings = append(warnings, "aws passthrough: "+err.Error())
			}
			for _, p := range profiles {
				if config.ValidateTarget(p.Name) == nil {
					add(p.Name, p.Region)
				}
			}
		}
		for _, id := range cfg.ByTool(config.ToolAWS) {
			add(id.Target, id.Region)
		}
		var b strings.Builder
		b.WriteString("# Generated by `iris install`. Claude-only AWS config.\n")
		for _, name := range order {
			if name == "default" {
				b.WriteString("[default]\n")
			} else {
				fmt.Fprintf(&b, "[profile %s]\n", name)
			}
			fmt.Fprintf(&b, "credential_process = %s aws %s\n", shellQuote(o.HelperBin), name)
			if r := regions[name]; r != "" {
				fmt.Fprintf(&b, "region = %s\n", r)
			}
			b.WriteString("\n")
		}
		if err := write(paths.ClaudeAWSConfig(), []byte(b.String()), 0o644); err != nil {
			return written, err
		}
	}

	// gcloud: config dir + shims
	if cfg.Uses(config.ToolGCP) {
		project := ""
		if ids := cfg.ByTool(config.ToolGCP); len(ids) > 0 {
			project = ids[0].Target
		} else {
			project = GcloudProject()
		}
		props := "[core]\ndisable_usage_reporting = true\n"
		if project != "" {
			props += "project = " + project + "\n"
		}
		if err := write(filepath.Join(paths.ClaudeGcloudDir(), "properties"), []byte(props), 0o644); err != nil {
			return written, err
		}
		for _, bin := range []string{"gcloud", "bq", "gsutil"} {
			sh := fmt.Sprintf("#!/bin/sh\n# Generated by iris: run %s with a brokered, short-lived token.\nexec %s gcloud-shim %s \"$@\"\n", bin, shellQuote(o.HelperBin), bin)
			if err := write(filepath.Join(paths.ShimDir(), bin), []byte(sh), 0o755); err != nil {
				return written, err
			}
		}
	}

	// git
	if cfg.Uses(config.ToolGit) {
		var b strings.Builder
		b.WriteString("# Generated by `iris install`. Claude-only global git config.\n")
		if _, err := os.Stat(filepath.Join(paths.Home(), ".gitconfig")); err == nil {
			b.WriteString("[include]\n\tpath = ~/.gitconfig\n")
		}
		// An empty helper resets any helper from ~/.gitconfig (osxkeychain etc.).
		fmt.Fprintf(&b, "[credential]\n\thelper =\n\thelper = %s git\n\tuseHttpPath = false\n", shellQuote(o.HelperBin))
		hosts := []string{}
		if cfg.PassthroughEnabled(config.ToolGit) {
			hosts = append(hosts, "github.com", "gitlab.com", "bitbucket.org")
		}
		for _, id := range cfg.ByTool(config.ToolGit) {
			hosts = append(hosts, id.Target)
		}
		seen := map[string]bool{}
		for _, h := range hosts {
			if seen[h] {
				continue
			}
			seen[h] = true
			// SSH keys are hidden from the sandbox: rewrite SSH remotes to HTTPS.
			fmt.Fprintf(&b, "[url \"https://%s/\"]\n\tinsteadOf = git@%s:\n\tinsteadOf = ssh://git@%s/\n", h, h, h)
		}
		if err := write(paths.ClaudeGitconfig(), []byte(b.String()), 0o644); err != nil {
			return written, err
		}
	}

	// docker
	if cfg.Uses(config.ToolDocker) {
		dc := map[string]any{}
		if cfg.PassthroughEnabled(config.ToolDocker) {
			dc["credsStore"] = "iris" // every registry
		}
		if ids := cfg.ByTool(config.ToolDocker); len(ids) > 0 {
			helpers := map[string]string{}
			for _, id := range ids {
				helpers[id.Target] = "iris"
			}
			dc["credHelpers"] = helpers
		}
		b, _ := json.MarshalIndent(dc, "", "  ")
		if err := write(filepath.Join(paths.ClaudeDockerDir(), "config.json"), b, 0o644); err != nil {
			return written, err
		}
		link := filepath.Join(paths.ShimDir(), "docker-credential-iris")
		_ = os.Remove(link)
		if err := os.Symlink(o.HelperBin, link); err != nil {
			return written, err
		}
		written = append(written, link)
	}

	// helper on PATH for convenience (iris-helper ping)
	link := filepath.Join(paths.ShimDir(), "iris-helper")
	_ = os.Remove(link)
	if err := os.Symlink(o.HelperBin, link); err == nil {
		written = append(written, link)
	}

	if cfg.Proxy.Enabled {
		if err := writeCABundle(write); err != nil {
			warnings = append(warnings, "CA bundle: "+err.Error())
		}
	}

	if len(warnings) > 0 {
		sort.Strings(warnings)
		return written, &Warnings{List: warnings}
	}
	return written, nil
}

type Warnings struct{ List []string }

func (w *Warnings) Error() string { return strings.Join(w.List, "\n") }

func writeCABundle(write func(string, []byte, os.FileMode) error) error {
	ca, err := os.ReadFile(paths.CACert())
	if err != nil {
		return fmt.Errorf("no Iris CA yet (%v)", err)
	}
	var roots []byte
	if runtime.GOOS == "darwin" {
		roots, err = exec.Command("security", "find-certificate", "-a", "-p",
			"/System/Library/Keychains/SystemRootCertificates.keychain").Output()
		if err != nil {
			return fmt.Errorf("exporting system roots: %w", err)
		}
	} else {
		for _, p := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"} {
			if b, err := os.ReadFile(p); err == nil {
				roots = b
				break
			}
		}
	}
	return write(paths.ClaudeCABundle(), append(append(roots, '\n'), ca...), 0o644)
}
