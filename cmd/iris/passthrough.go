package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/install"
	"github.com/mechanomythia/iris/internal/paths"
)

const passthroughUsage = `iris passthrough on|off|status [tools]

Lets Claude use YOUR existing credentials (kube contexts, AWS profiles,
gcloud, git and docker credential helpers). The sandbox still hides them;
every use shows the iris dialog first and runs only if you allow it.

  iris passthrough on                 all tools: kube, aws, gcp, git, docker
  iris passthrough on kube,aws        only these
  iris passthrough off                Claude-only identities only (iris provision)
  iris passthrough off git            stop passing through one tool
  iris passthrough status

Explicit identities in the config (iris provision) still take precedence
for their target. Re-run after adding kube contexts or AWS profiles:
  iris install
`

func cmdPassthrough(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(passthroughUsage)
		return nil
	}
	if _, err := install.EnsureConfig(""); err != nil {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	tools := config.PassthroughTools
	if len(args) > 1 {
		tools = splitList(args[1])
		for _, t := range tools {
			if !contains(config.PassthroughTools, t) {
				return fmt.Errorf("unknown tool %q (want %s)", t, strings.Join(config.PassthroughTools, ", "))
			}
		}
	}
	switch args[0] {
	case "status":
		if len(cfg.Passthrough.Tools) == 0 {
			fmt.Println("passthrough: off (Claude can only use identities from `iris provision`)")
		} else {
			fmt.Println("passthrough: on for", strings.Join(cfg.Passthrough.Tools, ", "))
		}
		for _, id := range cfg.Identities {
			fmt.Printf("  explicit identity: %s\n", id.Label())
		}
		return nil
	case "on":
		for _, t := range tools {
			if !contains(cfg.Passthrough.Tools, t) {
				cfg.Passthrough.Tools = append(cfg.Passthrough.Tools, t)
			}
		}
	case "off":
		if len(args) == 1 {
			cfg.Passthrough.Tools = nil
		} else {
			var keep []string
			for _, t := range cfg.Passthrough.Tools {
				if !contains(tools, t) {
					keep = append(keep, t)
				}
			}
			cfg.Passthrough.Tools = keep
		}
	default:
		fmt.Print(passthroughUsage)
		return errors.New("expected on, off or status")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.Save("", cfg); err != nil {
		return err
	}
	if len(cfg.Passthrough.Tools) == 0 {
		fmt.Println("passthrough off")
	} else {
		fmt.Println("passthrough on for", strings.Join(cfg.Passthrough.Tools, ", "))
	}
	fmt.Println("updating Claude Code configuration in", paths.Tilde(paths.ConfigFile()), "…")
	return runInstall(false, false, nil, false)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
