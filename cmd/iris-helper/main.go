// iris-helper runs inside the Claude Code sandbox. It holds no secrets and
// only talks to broker.sock. Each subcommand speaks one CLI's credential
// helper protocol.
//
//	iris-helper kube <context>        kubeconfig users[].user.exec
//	iris-helper aws <profile>         ~/.aws/config credential_process
//	iris-helper gcp <project>         prints an access token
//	iris-helper gcloud-shim <bin> ... runs gcloud/bq/gsutil with a brokered token
//	iris-helper git get|store|erase   git credential helper
//	iris-helper docker get|store|erase|list   (also as docker-credential-iris)
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mechanomythia/iris/internal/creds"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/protocol"
)

func main() {
	args := os.Args[1:]
	if strings.HasPrefix(filepath.Base(os.Args[0]), "docker-credential-") {
		args = append([]string{"docker"}, args...)
	}
	if len(args) == 0 {
		usage()
	}
	var err error
	switch args[0] {
	case "kube":
		err = kube(args[1:])
	case "aws":
		err = aws(args[1:])
	case "gcp":
		err = gcp(args[1:])
	case "gcloud-shim":
		err = gcloudShim(args[1:])
	case "git":
		err = git(args[1:])
	case "docker":
		err = docker(args[1:])
	case "ping":
		_, err = protocol.Call(paths.BrokerSocket(), protocol.Request{Op: "ping"}, 5*time.Second)
		if err == nil {
			fmt.Println("broker reachable at", paths.BrokerSocket())
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "iris:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: iris-helper kube|aws|gcp|gcloud-shim|git|docker|ping ...")
	os.Exit(2)
}

func request(tool, target string) (*protocol.Credential, error) {
	cwd, _ := os.Getwd()
	req := protocol.Request{Op: "credential", Tool: tool, Target: target,
		PID: os.Getpid(), PPID: os.Getppid(), Cwd: cwd, Argv: os.Args}
	// Long timeout: a human may be looking at a popup.
	resp, err := protocol.Call(paths.BrokerSocket(), req, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Iris broker at %s: %w (is irisd running? is the socket in sandbox.network.allowUnixSockets?)", paths.BrokerSocket(), err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Credential, nil
}

func one(args []string, what string) (string, error) {
	if len(args) != 1 || args[0] == "" {
		return "", fmt.Errorf("expected exactly one %s", what)
	}
	return args[0], nil
}

func kube(args []string) error {
	ctx, err := one(args, "kube context")
	if err != nil {
		return err
	}
	c, err := request("kube", ctx)
	if err != nil {
		return err
	}
	b, err := creds.KubeExecCredential(c)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func aws(args []string) error {
	profile, err := one(args, "aws profile")
	if err != nil {
		return err
	}
	c, err := request("aws", profile)
	if err != nil {
		return err
	}
	b, err := creds.AWSProcess(c)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func gcp(args []string) error {
	project, err := one(args, "gcp project")
	if err != nil {
		return err
	}
	c, err := request("gcp", project)
	if err != nil {
		return err
	}
	fmt.Println(c.Token)
	return nil
}

// gcloudShim: gcloud has no exec-style hook, so a shim on PATH (inside
// Claude sessions only) fetches a token, writes it to a private temp file
// and runs the real binary with CLOUDSDK_AUTH_ACCESS_TOKEN_FILE.
func gcloudShim(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("gcloud-shim: missing binary name")
	}
	name, rest := args[0], args[1:]
	real, err := findReal(name)
	if err != nil {
		return err
	}
	project := projectFromArgs(rest)
	if project == "" {
		project = os.Getenv("CLOUDSDK_CORE_PROJECT")
	}
	if project == "" {
		project = os.Getenv("IRIS_GCP_DEFAULT_PROJECT")
	}
	if project == "" || passthrough(rest) {
		return run(real, rest, nil)
	}
	c, err := request("gcp", project)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "iris-gcp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(c.Token); err != nil {
		return err
	}
	f.Close()
	return run(real, rest, []string{"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=" + f.Name(), "CLOUDSDK_CORE_PROJECT=" + project})
}

// passthrough: commands that need no credential.
func passthrough(a []string) bool {
	if len(a) == 0 {
		return true
	}
	switch a[0] {
	case "version", "--version", "help", "--help", "config", "topic", "info":
		return true
	}
	return false
}

func projectFromArgs(a []string) string {
	for i, s := range a {
		if s == "--project" && i+1 < len(a) {
			return a[i+1]
		}
		if strings.HasPrefix(s, "--project=") {
			return strings.TrimPrefix(s, "--project=")
		}
	}
	return ""
}

// findReal looks up name on PATH, skipping the Iris shim directory.
func findReal(name string) (string, error) {
	shim, _ := filepath.Abs(paths.ShimDir())
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if d, _ := filepath.Abs(dir); d == shim {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH (outside %s)", name, shim)
}

func run(bin string, args, extraEnv []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), extraEnv...)
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	return err
}

// ---- git credential helper protocol ----

func readKV(r io.Reader) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func git(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("git: expected get|store|erase")
	}
	in := readKV(os.Stdin)
	if args[0] != "get" {
		return nil // never store anything; the broker owns secrets
	}
	host := in["host"]
	if host == "" {
		return nil
	}
	c, err := request("git", host)
	if err != nil {
		// Print to stderr and return nothing: git then fails auth instead of
		// falling through to an interactive prompt.
		fmt.Fprintln(os.Stderr, "iris:", err)
		fmt.Println("quit=1")
		return nil
	}
	_, err = os.Stdout.Write(creds.GitCredential(c, ""))
	return err
}

// ---- docker credential helper protocol ----

func docker(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("docker: expected get|store|erase|list")
	}
	switch args[0] {
	case "list":
		fmt.Println("{}")
		return nil
	case "store", "erase":
		_, _ = io.Copy(io.Discard, os.Stdin)
		return nil
	case "get":
		raw, _ := io.ReadAll(os.Stdin)
		server := strings.TrimSpace(string(raw))
		host := server
		if u, err := url.Parse(server); err == nil && u.Host != "" {
			host = u.Host
		}
		c, err := request("docker", host)
		if err != nil {
			return err
		}
		b, err := creds.DockerCredential(c, server)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	return fmt.Errorf("docker: unknown action %q", args[0])
}
