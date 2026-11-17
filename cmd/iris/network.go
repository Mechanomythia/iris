package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mechanomythia/iris/internal/config"
	"github.com/mechanomythia/iris/internal/install"
	"github.com/mechanomythia/iris/internal/proxy"
)

const networkUsage = `iris network on|off|status|allow HOST|forget HOST

Makes iris the sandbox's network gate. Claude Code sends sandbox traffic
through the iris proxy (HTTP/HTTPS) and SOCKS (SSH on port 22, anything
else); a host that isn't allowed yet shows the iris dialog with the
command that tried to reach it: Deny, Once, 15 min, 1 h or Always (saved).

  iris network on             enable (asks for unknown hosts)
  iris network allow gitlab.com   allow without asking (globs: *.example.com)
  iris network forget gitlab.com
  iris network status
  iris network off            back to Claude Code's own allowlist
`

func cmdNetwork(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(networkUsage)
		return nil
	}
	if _, err := install.EnsureConfig(""); err != nil {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	save := func(reinstall bool) error {
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := config.Save("", cfg); err != nil {
			return err
		}
		if reinstall {
			return runInstall(false, false, nil, false)
		}
		return reloadQuiet()
	}
	switch args[0] {
	case "on":
		cfg.Proxy.Enabled = true
		cfg.Proxy.UnknownHosts = "ask"
		if cfg.Proxy.Listen == "" {
			cfg.Proxy.Listen = "127.0.0.1:18899"
		}
		if cfg.Proxy.SocksListen == "" {
			cfg.Proxy.SocksListen = "127.0.0.1:18898"
		}
		fmt.Println("network gate on: unknown hosts ask first (HTTP proxy " + cfg.Proxy.Listen + ", SOCKS " + cfg.Proxy.SocksListen + ")")
		if err := save(true); err != nil {
			return err
		}
		fmt.Println("restart the broker (iris restart) and your Claude sessions to route traffic through iris")
		return nil
	case "off":
		cfg.Proxy.UnknownHosts = ""
		cfg.Proxy.SocksListen = ""
		if len(cfg.ByTool(config.ToolHTTP)) == 0 {
			cfg.Proxy.Enabled = false
		}
		fmt.Println("network gate off: Claude Code's own allowlist applies again")
		return save(true)
	case "status":
		if !cfg.Proxy.Asks() {
			fmt.Println("network gate: off (iris network on)")
		} else {
			fmt.Println("network gate: on, unknown hosts ask")
		}
		fmt.Println("allowed by you:")
		for _, h := range cfg.Proxy.AllowedHosts {
			fmt.Println("  " + h)
		}
		if len(cfg.Proxy.AllowedHosts) == 0 {
			fmt.Println("  (none yet)")
		}
		if !cfg.Proxy.NoDefaults {
			fmt.Printf("built-in: %s\n", strings.Join(proxy.DefaultAllowed, " "))
		}
		return nil
	case "allow", "forget":
		if len(args) != 2 {
			return errors.New("usage: iris network " + args[0] + " HOST")
		}
		h := strings.ToLower(args[1])
		var keep []string
		found := false
		for _, x := range cfg.Proxy.AllowedHosts {
			if x == h {
				found = true
				if args[0] == "forget" {
					continue
				}
			}
			keep = append(keep, x)
		}
		if args[0] == "allow" && !found {
			keep = append(keep, h)
		}
		cfg.Proxy.AllowedHosts = keep
		fmt.Printf("%s %s\n", map[string]string{"allow": "allowed", "forget": "forgot"}[args[0]], h)
		return save(false)
	}
	fmt.Print(networkUsage)
	return fmt.Errorf("unknown command %q", args[0])
}

func reloadQuiet() error {
	if err := control(protocolReload(), nil); err != nil {
		fmt.Println("(broker not reachable; it picks up the change when it starts)")
	}
	return nil
}
