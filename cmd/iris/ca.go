package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/paths"
	"github.com/mechanomythia/iris/internal/proxy"
)

const caUsage = `iris ca trust|untrust|status

The proxy injects tokens (e.g. your GitLab token for glab) by terminating
HTTPS for those hosts with the iris CA. Most tools in the sandbox trust it
through the CA bundle iris sets; Go tools on macOS (glab, gh, terraform,
kubectl) only trust your keychain, so trust it there once:

  iris ca trust      add the iris CA to your login keychain (asks for your password)
  iris ca untrust    remove it (and older iris CAs)
  iris ca status

The CA is name-constrained to the hosts you inject for (proxy http
identities): even trusted, it can't vouch for any other site.
`

func loginKeychain() string {
	return filepath.Join(paths.Home(), "Library", "Keychains", "login.keychain-db")
}

func cmdCA(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(caUsage)
		return nil
	}
	if runtime.GOOS != "darwin" {
		return errors.New("iris ca manages the macOS keychain")
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		ca, _, err := proxy.EnsureCA(paths.CACert(), paths.CAKey(), cfg.HTTPHosts())
		if err != nil {
			return err
		}
		fmt.Println("iris CA valid only for:", strings.Join(ca.Hosts(), ", "))
		if exec.Command("security", "verify-cert", "-c", paths.CACert(), "-p", "ssl", "-L").Run() == nil {
			fmt.Println("trusted in your keychain: yes")
		} else {
			fmt.Println("trusted in your keychain: no (iris ca trust)")
		}
		return nil
	case "untrust":
		n := removeIrisCAs()
		fmt.Printf("removed %d iris CA(s) from the login keychain\n", n)
		return nil
	case "trust":
		if len(cfg.HTTPHosts()) == 0 {
			return errors.New("no http identities yet: nothing needs the CA (e.g. iris provision gitlab)")
		}
		ca, _, err := proxy.EnsureCA(paths.CACert(), paths.CAKey(), cfg.HTTPHosts())
		if err != nil {
			return err
		}
		removeIrisCAs()
		fmt.Println("Adding the iris CA (valid only for " + strings.Join(ca.Hosts(), ", ") + ") to your login keychain.")
		fmt.Println("macOS asks for your password or Touch ID.")
		c := exec.Command("security", "add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", loginKeychain(), paths.CACert())
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("security add-trusted-cert: %w", err)
		}
		fmt.Println("trusted. Restart the broker so it uses this CA: iris restart")
		return nil
	}
	fmt.Print(caUsage)
	return fmt.Errorf("unknown command %q", args[0])
}

// removeIrisCAs deletes every iris CA from the login keychain (by name).
func removeIrisCAs() int {
	out, err := exec.Command("security", "find-certificate", "-a", "-c", proxy.CommonNamePrefix, "-Z", loginKeychain()).Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(line), "SHA-1 hash: "); ok {
			if exec.Command("security", "delete-certificate", "-Z", h, loginKeychain()).Run() == nil {
				n++
			}
		}
	}
	return n
}
