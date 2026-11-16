// Package secrets fetches raw secret material from a configured source.
// Everything here runs in the daemon, on the host, outside the sandbox.
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mechanomythia/iris/internal/config"
)

// Runner is swappable in tests.
var Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Never inherit a stdin: a command waiting on a TTY would hang the broker.
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out, nil
}

func Fetch(ctx context.Context, s config.Source) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	switch {
	case len(s.Command) > 0:
		return Runner(ctx, s.Command[0], s.Command[1:]...)
	case s.Op != "":
		return Runner(ctx, "op", "read", "--no-newline", s.Op)
	case s.Keychain != nil:
		return Runner(ctx, "security", "find-generic-password", "-w", "-s", s.Keychain.Service, "-a", s.Keychain.Account)
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return nil, fmt.Errorf("env %s not set in daemon environment", s.Env)
		}
		return []byte(v), nil
	case s.File != "":
		return os.ReadFile(expand(s.File))
	case s.Static != "":
		return []byte(s.Static), nil
	}
	return nil, errors.New("empty source")
}

// StoreKeychain saves a generic password (used by provisioners).
func StoreKeychain(ctx context.Context, service, account, secret string) error {
	// -U updates if present. The secret goes via argv which is visible to
	// local `ps` for a moment; acceptable under the threat model (host is trusted).
	_, err := Runner(ctx, "security", "add-generic-password", "-U", "-s", service, "-a", account, "-w", secret)
	return err
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return h + p[1:]
	}
	return p
}
