package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/mechanomythia/iris/internal/cloudenv"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/provision"
)

const cloudUsage = `iris cloud <command>   set up Claude Code cloud environments (claude.ai/code)

  new [name] [--dry-run] [--no-create]   interactive: pick access, create identities, walk through the dialog
  show <name> [--print]                   walk through (or print) a saved environment again
  list                                    saved environments

There is no API for cloud environments, so iris prepares everything and
walks you through pasting it into the environment dialog, copying each part
to the clipboard.
`

func cmdCloud(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(cloudUsage)
		return nil
	}
	switch args[0] {
	case "new":
		return cloudNew(args[1:], os.Stdin, os.Stdout)
	case "show":
		return cloudShow(args[1:], os.Stdin, os.Stdout)
	case "list":
		entries, _ := os.ReadDir(cloudDir(""))
		if len(entries) == 0 {
			fmt.Println("no saved environments (iris cloud new)")
		}
		for _, e := range entries {
			if e.IsDir() {
				fmt.Println(" ", e.Name())
			}
		}
		return nil
	}
	fmt.Print(cloudUsage)
	return fmt.Errorf("unknown command %q", args[0])
}

func cloudDir(name string) string { return filepath.Join(paths.ConfigDir(), "cloud", name) }

// ---------------- prompts ----------------

type prompter struct {
	in  *bufio.Reader
	out io.Writer
	tty *os.File // for hidden input; nil in tests
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	p := &prompter{in: bufio.NewReader(in), out: out}
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			p.tty = f
		}
	}
	return p
}

func (p *prompter) line() string {
	s, _ := p.in.ReadString('\n')
	return strings.TrimSpace(s)
}

func (p *prompter) ask(q, def string) string {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", q)
	}
	if s := p.line(); s != "" {
		return s
	}
	return def
}

func (p *prompter) need(q, def string) string {
	for {
		if s := p.ask(q, def); s != "" {
			return s
		}
		fmt.Fprintln(p.out, "  required")
	}
}

func (p *prompter) yes(q string, def bool) bool {
	d := "Y/n"
	if !def {
		d = "y/N"
	}
	fmt.Fprintf(p.out, "%s [%s]: ", q, d)
	s := strings.ToLower(p.line())
	if s == "" {
		return def
	}
	return s == "y" || s == "yes"
}

func (p *prompter) access(what string) cloudenv.Access {
	for {
		s := strings.ToLower(p.ask("  "+what+" access: read or write", "read"))
		switch s {
		case "r", "read":
			return cloudenv.Read
		case "w", "write":
			return cloudenv.Write
		}
		fmt.Fprintln(p.out, "  type read or write")
	}
}

// multi asks for comma-separated numbers from a numbered list.
func (p *prompter) multi(q string, opts []string) []int {
	for i, o := range opts {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, o)
	}
	for {
		s := p.ask(q, "")
		var out []int
		ok := true
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 1 || n > len(opts) {
				ok = false
				break
			}
			out = append(out, n-1)
		}
		if ok && len(out) > 0 {
			return out
		}
		fmt.Fprintf(p.out, "  numbers between 1 and %d, comma separated\n", len(opts))
	}
}

func (p *prompter) secret(q string) string {
	fmt.Fprint(p.out, q+": ")
	if p.tty != nil {
		stty := func(a string) { c := exec.Command("stty", a); c.Stdin = p.tty; _ = c.Run() }
		stty("-echo")
		defer func() { stty("echo"); fmt.Fprintln(p.out) }()
	}
	return p.line()
}

func (p *prompter) pause(msg string) {
	fmt.Fprintf(p.out, "%s  [Enter when done] ", msg)
	p.line()
}

// copyClip puts text on the clipboard (macOS pbcopy, else xclip/wl-copy).
// Returns false when no clipboard tool exists; the caller prints instead.
var copyClip = func(text string) bool {
	for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(text)
			if cmd.Run() == nil {
				return true
			}
		}
	}
	return false
}

// ---------------- iris cloud new ----------------

type savedEnv struct {
	Request cloudenv.Request `json:"request"`
}

func cloudNew(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("cloud new", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the identity commands, create nothing")
	noCreate := fs.Bool("no-create", false, "don't create identities; leave placeholders in the guide")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := newPrompter(in, out)
	ctx := context.Background()

	fmt.Fprintln(out, "iris cloud new: a Claude Code cloud environment with exactly the access you choose.")
	fmt.Fprintln(out)
	def := fs.Arg(0)
	if def == "" {
		if wd, err := os.Getwd(); err == nil {
			def = filepath.Base(wd)
		}
	}
	req := cloudenv.Request{Name: p.need("Environment name (shown in Claude's environment menu)", def)}

	services := []string{
		"GitHub repositories (built in: the GitHub proxy, no credential)",
		"Google Cloud (gcloud, client libraries)",
		"Firebase",
		"Kubernetes on GKE",
		"Artifact Registry (container images on pkg.dev)",
		"AWS",
		"Docker registry (Docker Hub or GHCR)",
		"HTTP APIs with a token (OpenAI, Stripe, Cloudflare, ...)",
	}
	fmt.Fprintln(out, "\nWhat does Claude need?")
	gcloudProject := ""
	if b, err := exec.Command("gcloud", "config", "get-value", "project").Output(); err == nil {
		gcloudProject = strings.TrimSpace(string(b))
	}
	project := func(prev string) string {
		if prev == "" {
			prev = gcloudProject
		}
		return p.need("  GCP project", prev)
	}
	lastProject := ""
	for _, i := range p.multi("Pick numbers (e.g. 1,2,6)", services) {
		switch i {
		case 0:
			req.GitHub = true
		case 1:
			fmt.Fprintln(out, "\nGoogle Cloud")
			g := &cloudenv.GCP{Project: project(lastProject)}
			lastProject = g.Project
			g.Access = p.access("GCP")
			roles := p.ask("  roles (comma separated)", strings.Join(cloudenv.GCPRoles(g.Access), ","))
			g.Roles = splitList(roles)
			req.GCP = g
		case 2:
			fmt.Fprintln(out, "\nFirebase")
			f := &cloudenv.Firebase{Project: project(lastProject)}
			lastProject = f.Project
			f.Access = p.access("Firebase")
			req.Firebase = f
		case 3:
			fmt.Fprintln(out, "\nKubernetes on GKE")
			k := &cloudenv.GKE{Project: project(lastProject)}
			lastProject = k.Project
			k.Cluster = p.need("  cluster name", "")
			k.Location = p.need("  cluster location (region or zone)", "")
			k.Access = p.access("Kubernetes")
			req.GKE = k
		case 4:
			fmt.Fprintln(out, "\nArtifact Registry")
			a := &cloudenv.ArtifactRegistry{Project: project(lastProject)}
			lastProject = a.Project
			a.Location = p.need("  repository location (e.g. europe-west4)", "")
			a.Access = p.access("Artifact Registry")
			req.Registry = a
		case 5:
			fmt.Fprintln(out, "\nAWS")
			a := &cloudenv.AWS{Region: p.need("  region", "eu-west-1")}
			a.Access = p.access("AWS")
			pols := p.ask("  policies (comma separated ARNs)", strings.Join(cloudenv.AWSPolicies(a.Access), ","))
			a.Policies = splitList(pols)
			req.AWS = a
		case 6:
			fmt.Fprintln(out, "\nDocker registry")
			d := &cloudenv.Docker{}
			if p.ask("  registry: docker.io or ghcr.io", "docker.io") == "ghcr.io" {
				d.Registry = "ghcr.io"
			} else {
				d.Registry = "docker.io"
			}
			d.Username = p.need("  username", "")
			d.Access = p.access("Registry")
			req.Docker = d
		case 7:
			fmt.Fprintln(out, "\nHTTP APIs (each becomes a network secret: the token never enters the VM)")
			var names []string
			for _, a := range cloudenv.APIPresets {
				names = append(names, fmt.Sprintf("%-13s %s", a.Name, a.Host))
			}
			names = append(names, "Other (enter host and header)")
			for _, j := range p.multi("Pick numbers", names) {
				if j < len(cloudenv.APIPresets) {
					req.APIs = append(req.APIs, cloudenv.APIPresets[j])
					continue
				}
				a := cloudenv.API{Name: p.need("  name", ""), Host: p.need("  host (e.g. api.example.com)", "")}
				a.Header = p.ask("  header", "Authorization")
				a.Prefix = p.ask("  prefix before the token (empty for none)", "Bearer")
				req.APIs = append(req.APIs, a)
			}
		}
	}

	// Identities.
	var creds cloudenv.Credentials
	needGCP := len(req.GCPProjects()) > 0
	if (needGCP || req.AWS != nil || req.Docker != nil) && !*noCreate {
		fmt.Fprintln(out, "\nIdentities for this environment (dedicated and narrow, so you can revoke them on their own):")
		if needGCP {
			fmt.Fprintf(out, "  - GCP service account claude-%s with %s, plus a key\n", req.Name, describeRoles(req.GCPProjects()))
		}
		if req.AWS != nil {
			fmt.Fprintf(out, "  - AWS IAM user iris-claude-%s with %s, plus an access key\n", req.Name, strings.Join(req.AWS.Policies, ", "))
		}
		if req.Docker != nil {
			fmt.Fprintf(out, "  - a %s token you create (iris opens the page)\n", req.Docker.Registry)
		}
		if p.yes("Create them now with your own gcloud/aws login?", true) {
			r := provision.NewRunner(*dry, out)
			if needGCP {
				email, key, err := cloudenv.CreateGCP(ctx, r, req)
				if err != nil {
					return err
				}
				creds.GCPServiceAcc, creds.GCPKeyB64 = email, key
			}
			if req.AWS != nil {
				profile := p.ask("  AWS profile to create the user with", os.Getenv("AWS_PROFILE"))
				user, id, sec, err := cloudenv.CreateAWS(ctx, r, req, profile)
				if err != nil {
					return err
				}
				creds.AWSUser, creds.AWSKeyID, creds.AWSSecret = user, id, sec
			}
			if req.Docker != nil && !*dry {
				u := cloudenv.DockerTokenURL(*req.Docker, req.Name)
				fmt.Fprintf(out, "\nCreate a %s token (%s access):\n  %s\n", req.Docker.Registry, req.Docker.Access, u)
				openURL(u)
				creds.DockerToken = p.secret("Paste the token (input hidden)")
			}
		}
	}
	if *dry {
		fmt.Fprintln(out, "\n(dry-run) nothing created; the guide below has placeholders.")
	}

	g := cloudenv.Build(req, creds)
	dir, err := saveCloud(req, creds, g)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nSaved to %s (guide.md, setup.sh; secrets.json is private: mode 0600).\n", paths.Tilde(dir))
	if p.yes("\nWalk through adding it at claude.ai/code now?", true) {
		walkthrough(p, g)
	}
	return nil
}

func describeRoles(m map[string][]string) string {
	var parts []string
	for p, roles := range m {
		parts = append(parts, strings.Join(roles, ", ")+" on "+p)
	}
	return strings.Join(parts, "; ")
}

func openURL(u string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	_ = exec.Command(opener, u).Start()
}

func saveCloud(req cloudenv.Request, c cloudenv.Credentials, g cloudenv.Guide) (string, error) {
	dir := cloudDir(sanitizeName(req.Name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	rb, _ := json.MarshalIndent(savedEnv{Request: req}, "", "  ")
	cb, _ := json.MarshalIndent(c, "", "  ")
	for name, data := range map[string][]byte{
		"request.json": rb, "guide.md": []byte(g.Markdown()), "setup.sh": []byte(g.SetupScript),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), cb, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func loadCloud(name string) (cloudenv.Request, cloudenv.Credentials, error) {
	dir := cloudDir(sanitizeName(name))
	var s savedEnv
	var c cloudenv.Credentials
	b, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return s.Request, c, fmt.Errorf("no saved environment %q (iris cloud list)", name)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s.Request, c, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "secrets.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return s.Request, c, nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == 0 || r == '.' && b.Len() == 0 {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cloudShow(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("cloud show", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "print the guide (secrets redacted) instead of walking through it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: iris cloud show <name> [--print]")
	}
	req, c, err := loadCloud(fs.Arg(0))
	if err != nil {
		return err
	}
	g := cloudenv.Build(req, c)
	if *printOnly {
		fmt.Fprint(out, g.Markdown())
		return nil
	}
	walkthrough(newPrompter(in, out), g)
	return nil
}

// ---------------- the walkthrough ----------------

func walkthrough(p *prompter, g cloudenv.Guide) {
	out := p.out
	step := 0
	section := func(title string) {
		step++
		fmt.Fprintf(out, "\n── %d. %s ─────────────────────────────\n", step, title)
	}
	give := func(label, text string, secret bool) {
		if copyClip(text) {
			fmt.Fprintf(out, "Copied %s to the clipboard.\n", label)
			return
		}
		if secret {
			fmt.Fprintf(out, "No clipboard tool found; %s (contains secrets):\n", label)
		} else {
			fmt.Fprintf(out, "%s:\n", label)
		}
		fmt.Fprintln(out, indent(text))
	}

	for _, w := range g.Warnings {
		fmt.Fprintln(out, "! "+w)
	}
	section("Open the dialog")
	fmt.Fprintln(out, "At claude.ai/code, on the new-session screen, open the environment selector")
	fmt.Fprintln(out, "(first control under the message box) and choose New environment.")
	openURL("https://claude.ai/code")
	p.pause("")

	section("Name")
	give("the name", g.Name, false)
	p.pause("Paste it into Name.")

	section("Network access")
	fmt.Fprintf(out, "Choose %s and keep Allow package managers checked.\n", g.Network)
	if len(g.AllowedDomains) > 0 {
		give("the allowed domains", strings.Join(g.AllowedDomains, "\n"), false)
		p.pause("Paste them into Allowed domains.")
	} else {
		p.pause("No extra domains needed.")
	}

	if len(g.EnvVars) > 0 {
		section("Environment variables")
		give("the variables", g.EnvBlock(), true)
		p.pause("Paste them into Environment variables.")
	}

	section("Setup script")
	give("the setup script", g.SetupScript, false)
	p.pause("Paste it into Setup script.")

	section("Save")
	p.pause("Select Add environment.")

	if len(g.NetSecrets) > 0 {
		section("Network secrets")
		fmt.Fprintln(out, "Open the environment again (settings icon on its row) → Network secrets → Add secret, once per API.")
		for _, s := range g.NetSecrets {
			fmt.Fprintf(out, "\n%s\n", s.Service)
			give("its host", s.Host, false)
			fmt.Fprintf(out, "  Allowed websites: %s\n  Header name: %s\n", s.Host, s.Header)
			if s.Prefix == "" {
				fmt.Fprintln(out, "  Prefix: clear it (the token goes in bare)")
			} else {
				fmt.Fprintf(out, "  Prefix: %s\n", s.Prefix)
			}
			fmt.Fprintf(out, "  Value: paste your %s token\n", s.Service)
			p.pause("Select Add.")
		}
	}

	section("First session")
	if g.Repositories {
		fmt.Fprintln(out, "Start a session in this environment and select the repositories Claude should work on.")
	}
	give("a first message that checks every credential", g.FirstSession, false)
	fmt.Fprintln(out, "\nDone. Re-run this walkthrough any time: iris cloud show "+g.Name)
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
