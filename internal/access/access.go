// Package access decides whether a credential use is a read. Reads can be
// allowed in bulk ("reads on gitlab.com for 1 h"); everything else asks.
//
// HTTP is decided per request by its method, so a reads grant never lets a
// POST through. Tokens and signatures (kube, gcloud, aws, git, docker, SSH)
// are decided from the command Claude runs: a read only if every program in
// it is a known read or a harmless filter. Anything iris can't see into
// (scripts, interpreters, variables as commands) counts as a write.
package access

import (
	"path"
	"strings"

	"github.com/mechanomythia/iris/internal/shellfmt"
)

const (
	Read  = "read"
	Write = "write"
)

// HTTPMethod: GET, HEAD and OPTIONS are reads.
func HTTPMethod(method string) (string, string) {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return Read, strings.ToUpper(method) + " request"
	}
	return Write, strings.ToUpper(method) + " request"
}

// programs: the commands that use each tool's credential.
var programs = map[string][]string{
	"kube":   {"kubectl", "helm"},
	"aws":    {"aws", "terraform", "tofu"},
	"gcp":    {"gcloud", "gsutil", "bq", "terraform", "tofu"},
	"git":    {"git"},
	"docker": {"docker", "podman"},
	"ssh":    {"git", "ssh", "scp", "rsync", "sftp"},
}

// harmless: programs that can't run another program or use a credential.
var harmless = set("cd", "ls", "pwd", "echo", "printf", "cat", "head", "tail", "grep", "egrep", "fgrep", "rg",
	"jq", "yq", "sort", "uniq", "wc", "cut", "awk", "sed", "tr", "column", "tee", "less", "more", "true", "false",
	"sleep", "date", "base64", "diff", "test", "[", "[[", "basename", "dirname", "realpath", "export", "nl", "fold",
	"fmt", "paste", "rev", "comm", "join", "md5", "shasum", "sha256sum", "stat", "file", "which", "type")

// wrappers run the next word as the command; shellfmt marks it too.
var wrappers = set("sudo", "env", "time", "nohup", "command", "xargs", "exec")

// Command classifies a command line for one tool. ok=false: no command,
// or it doesn't mention the tool (callers treat that as a write).
func Command(tool, cmd string) (level, why string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Write, "no command reported"
	}
	progs := set(programs[tool]...)
	if len(progs) == 0 {
		return Write, "not a tool with reads"
	}
	var reads []string
	for _, words := range shellfmt.Commands(cmd) {
		prog, args, ok := program(words)
		if !ok {
			return Write, "runs " + firstText(words) + ", which iris can't see into"
		}
		if prog == "" {
			continue // assignments, loop headers
		}
		base := path.Base(prog)
		switch {
		case progs[base]:
			lvl, what := classify(tool, base, args)
			if lvl != Read {
				return Write, what
			}
			reads = append(reads, what)
		case harmless[base]:
		case otherTool(base):
			// gated by its own tool's prompts
		default:
			return Write, "runs " + base + ", which iris can't see into"
		}
	}
	if len(reads) == 0 {
		return Write, "the command doesn't run " + strings.Join(programs[tool], "/")
	}
	return Read, strings.Join(dedupe(reads), ", ")
}

func otherTool(p string) bool {
	for _, ps := range programs {
		for _, x := range ps {
			if x == p {
				return true
			}
		}
	}
	return p == "glab" || p == "gh" || p == "curl" || p == "wget" // http: decided per request
}

// program returns the command a simple command runs (after wrappers and
// assignments) and its arguments. ok=false when the command position holds
// something iris can't resolve ($VAR, a quoted string).
func program(words []shellfmt.Word) (string, []string, bool) {
	i := 0
	for i < len(words) {
		w := words[i]
		switch {
		case w.Kind == shellfmt.Keyword:
			i++
			if w.Text == "for" || w.Text == "case" {
				return "", nil, true // header: the rest is data
			}
			continue
		case w.Kind == shellfmt.Var && isAssignment(w.Text):
			i++
			continue
		case w.Kind == shellfmt.Cmd:
			if wrappers[w.Text] {
				i++
				for i < len(words) && words[i].Kind == shellfmt.Flag {
					i++
				}
				if w.Text == "env" {
					for i < len(words) && isAssignment(words[i].Text) {
						i++
					}
				}
				if i >= len(words) {
					return w.Text, nil, true
				}
				continue
			}
			return w.Text, texts(words[i+1:]), true
		case w.Kind == shellfmt.Arg && i > 0 && words[i-1].Kind == shellfmt.Keyword:
			return "", nil, true // "in a b c" of a for loop
		default:
			return "", nil, false
		}
	}
	return "", nil, true
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for _, c := range w[:eq] {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func texts(ws []shellfmt.Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = unquote(w.Text)
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' && s[len(s)-1] == '\'' || s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1]
	}
	return s
}

func firstText(ws []shellfmt.Word) string {
	if len(ws) == 0 {
		return "?"
	}
	return ws[0].Text
}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// positional drops flags (and the values of flags that take one).
func positional(args []string, valueFlags map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && valueFlags[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

var kubeValueFlags = set("-n", "--namespace", "--context", "--cluster", "--user", "-l", "--selector", "-o", "--output",
	"--kubeconfig", "-c", "--container", "-s", "--server", "--as", "--as-group", "--field-selector", "--sort-by",
	"--since", "--tail", "--template", "-f", "--filename", "-L", "--label-columns", "--request-timeout")

var awsValueFlags = set("--profile", "--region", "--output", "--query", "--endpoint-url", "--cli-read-timeout",
	"--cli-connect-timeout", "--color", "--ca-bundle")

var gitValueFlags = set("-C", "-c", "--git-dir", "--work-tree", "--namespace")

func classify(tool, prog string, args []string) (string, string) {
	switch prog {
	case "kubectl":
		return kubectl(args)
	case "helm":
		p := positional(args, kubeValueFlags)
		if len(p) > 0 && set("list", "ls", "status", "get", "history", "show", "search", "template", "version", "env", "lint")[p[0]] {
			return Read, "helm " + p[0]
		}
		return Write, "helm " + first(p)
	case "gcloud":
		return gcloud(args)
	case "gsutil":
		p := positional(args, nil)
		if len(p) > 0 && set("ls", "cat", "du", "stat", "hash", "version")[p[0]] {
			return Read, "gsutil " + p[0]
		}
		return Write, "gsutil " + first(p)
	case "bq":
		p := positional(args, nil)
		if len(p) > 0 && set("ls", "show", "head", "version")[p[0]] {
			return Read, "bq " + p[0]
		}
		return Write, "bq " + first(p)
	case "aws":
		return awsCLI(args)
	case "terraform", "tofu":
		p := positional(args, nil)
		if len(p) > 0 && (set("plan", "validate", "show", "version", "providers", "fmt")[p[0]] ||
			p[0] == "state" && len(p) > 1 && (p[1] == "list" || p[1] == "show")) {
			return Read, prog + " " + strings.Join(p[:min(2, len(p))], " ")
		}
		return Write, prog + " " + first(p)
	case "git":
		p := positional(args, gitValueFlags)
		if len(p) > 0 && set("fetch", "pull", "clone", "ls-remote", "log", "status", "diff", "show", "remote", "rev-parse", "branch", "describe", "blame")[p[0]] {
			return Read, "git " + p[0]
		}
		return Write, "git " + first(p)
	case "docker", "podman":
		p := positional(args, set("-H", "--host", "--context", "--config"))
		if len(p) > 0 && set("pull", "images", "inspect", "ps", "logs", "history", "search", "version", "info", "manifest", "image")[p[0]] {
			if p[0] == "image" || p[0] == "manifest" {
				if len(p) < 2 || !set("ls", "list", "inspect", "pull", "history")[p[1]] {
					return Write, prog + " " + strings.Join(p[:min(2, len(p))], " ")
				}
			}
			return Read, prog + " " + p[0]
		}
		return Write, prog + " " + first(p)
	case "ssh", "scp", "rsync", "sftp":
		return Write, prog + " (runs or copies on the remote)"
	}
	return Write, prog
}

func first(p []string) string {
	if len(p) == 0 {
		return ""
	}
	return p[0]
}

func kubectl(args []string) (string, string) {
	p := positional(args, kubeValueFlags)
	if len(p) == 0 {
		return Write, "kubectl"
	}
	verb := p[0]
	what := "kubectl " + verb
	switch verb {
	case "get", "describe":
		// Reading secrets hands out their values: never a bulk read.
		for _, r := range p[1:] {
			r = strings.ToLower(r)
			if strings.HasPrefix(r, "secret") || strings.Contains(r, "/secret") || strings.HasPrefix(r, "secrets/") {
				return Write, what + " secret (reveals values)"
			}
			if r == "all" {
				break
			}
		}
		return Read, what
	case "logs", "top", "explain", "api-resources", "api-versions", "version", "cluster-info", "events":
		return Read, what
	case "config":
		if len(p) > 1 && set("view", "get-contexts", "current-context", "get-clusters", "get-users")[p[1]] {
			return Read, what + " " + p[1]
		}
	case "auth":
		if len(p) > 1 && (p[1] == "can-i" || p[1] == "whoami") {
			return Read, what + " " + p[1]
		}
	case "rollout":
		if len(p) > 1 && (p[1] == "status" || p[1] == "history") {
			return Read, what + " " + p[1]
		}
	}
	return Write, what
}

var gcloudWrite = set("create", "delete", "update", "deploy", "set", "add-iam-policy-binding", "remove-iam-policy-binding",
	"set-iam-policy", "patch", "ssh", "scp", "start", "stop", "reset", "import", "export", "run", "submit", "cp", "rm",
	"mv", "rsync", "access", "print-access-token", "print-identity-token", "login", "activate-service-account",
	"get-credentials", "enable", "disable", "add", "remove", "restore", "resize", "execute", "call", "publish", "rollback")

var gcloudRead = set("list", "describe", "get-iam-policy", "read", "get-value", "tail", "info", "version", "get", "ls")

func gcloud(args []string) (string, string) {
	p := positional(args, set("--project", "--format", "--region", "--zone", "--filter", "--limit", "--location",
		"--account", "--configuration", "--sort-by", "--page-size", "--impersonate-service-account", "--freshness"))
	read := ""
	for _, w := range p {
		if gcloudWrite[w] {
			return Write, "gcloud … " + w
		}
		if gcloudRead[w] && read == "" {
			read = w
		}
	}
	if read != "" {
		return Read, "gcloud … " + read
	}
	return Write, "gcloud " + strings.Join(p[:min(2, len(p))], " ")
}

var awsSecretOps = set("get-secret-value", "get-parameter", "get-parameters", "get-parameters-by-path",
	"get-login-password", "get-session-token", "get-federation-token", "get-authorization-token", "get-role-credentials",
	"get-credentials-for-identity", "get-cluster-credentials", "get-temporary-credentials", "get-object")

func awsCLI(args []string) (string, string) {
	p := positional(args, awsValueFlags)
	if len(p) < 2 {
		return Write, "aws " + strings.Join(p, " ")
	}
	svc, op := p[0], p[1]
	what := "aws " + svc + " " + op
	if svc == "s3" {
		if op == "ls" {
			return Read, what
		}
		return Write, what
	}
	if awsSecretOps[op] {
		return Write, what + " (returns secrets)"
	}
	for _, pre := range []string{"describe-", "list-", "get-", "head-", "lookup-", "batch-get-", "search-"} {
		if strings.HasPrefix(op, pre) {
			return Read, what
		}
	}
	return Write, what
}
