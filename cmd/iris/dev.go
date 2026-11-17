package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mechanomythia/iris/internal/sparkle"
)

const devUsage = `iris dev <command>   release tooling (Sparkle auto-update signing)

  sparkle-keygen                 make the update-signing key; copies the private key to the clipboard
  sparkle-pubkey                 print SUPublicEDKey for $SPARKLE_ED_PRIVATE_KEY
  sparkle-sign FILE              print the edSignature of FILE with $SPARKLE_ED_PRIVATE_KEY
  appcast --version V --url U --file F [--notes URL] [--out appcast.xml]
`

func cmdDev(args []string) error {
	if len(args) == 0 {
		fmt.Print(devUsage)
		return nil
	}
	key := func() (string, error) {
		v := os.Getenv("SPARKLE_ED_PRIVATE_KEY")
		if v == "" {
			return "", errors.New("SPARKLE_ED_PRIVATE_KEY is not set")
		}
		return v, nil
	}
	switch args[0] {
	case "sparkle-keygen":
		priv, pub, err := sparkle.GenerateKey()
		if err != nil {
			return err
		}
		copied := false
		if p, err := exec.LookPath("pbcopy"); err == nil {
			c := exec.Command(p)
			c.Stdin = strings.NewReader(priv)
			copied = c.Run() == nil
		}
		fmt.Println("Public key (safe to share; CI derives it, nothing to do):")
		fmt.Println("  " + pub)
		if copied {
			fmt.Println("\nThe PRIVATE key is on your clipboard. Add it now as the GitHub secret")
			fmt.Println("SPARKLE_ED_PRIVATE_KEY (Settings → Secrets and variables → Actions),")
			fmt.Println("then keep a copy in your password manager. Anyone with it can sign updates.")
		} else {
			fmt.Println("\nPRIVATE key (add as GitHub secret SPARKLE_ED_PRIVATE_KEY, then store it safely):")
			fmt.Println("  " + priv)
		}
		return nil
	case "sparkle-pubkey":
		v, err := key()
		if err != nil {
			return err
		}
		k, err := sparkle.ParsePrivate(v)
		if err != nil {
			return err
		}
		fmt.Println(sparkle.PublicKey(k))
		return nil
	case "sparkle-sign":
		if len(args) != 2 {
			return errors.New("usage: iris dev sparkle-sign FILE")
		}
		v, err := key()
		if err != nil {
			return err
		}
		k, err := sparkle.ParsePrivate(v)
		if err != nil {
			return err
		}
		sig, _, err := sparkle.SignFile(k, args[1])
		if err != nil {
			return err
		}
		fmt.Println(sig)
		return nil
	case "appcast":
		fs := flag.NewFlagSet("appcast", flag.ExitOnError)
		version := fs.String("version", "", "version (CFBundleVersion)")
		url := fs.String("url", "", "download URL of the archive")
		file := fs.String("file", "", "the archive (signed and measured)")
		notes := fs.String("notes", "", "release notes URL")
		out := fs.String("out", "appcast.xml", "output file")
		_ = fs.Parse(args[1:])
		v, err := key()
		if err != nil {
			return err
		}
		k, err := sparkle.ParsePrivate(v)
		if err != nil {
			return err
		}
		sig, n, err := sparkle.SignFile(k, *file)
		if err != nil {
			return err
		}
		x, err := sparkle.Appcast("Iris", sparkle.Item{Version: *version, URL: *url, Length: n, EdSignature: sig,
			ReleaseNotesURL: *notes, MinimumSystem: "13.0"})
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, x, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (version %s, %d bytes signed)\n", *out, *version, n)
		return nil
	}
	fmt.Print(devUsage)
	return fmt.Errorf("unknown dev command %q", args[0])
}
