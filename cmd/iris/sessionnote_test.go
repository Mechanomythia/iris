package main

import (
	"strings"
	"testing"

	"github.com/mechanomythia/iris/internal/config"
)

func TestSessionNoteNamesPlaceholders(t *testing.T) {
	cfg := config.Default()
	if sessionNote(cfg) != "" {
		t.Fatal("no identities: no note")
	}
	cfg.Identities = []config.Identity{{Tool: "http", Target: "gitlab.com",
		SandboxEnv: map[string]string{"GITLAB_TOKEN": "iris-injects-the-real-token", "GITLAB_HOST": "gitlab.com"}}}
	n := sessionNote(cfg)
	if !strings.Contains(n, "GITLAB_TOKEN (gitlab.com)") || strings.Contains(n, "GITLAB_HOST") || !strings.Contains(n, "env -u") {
		t.Fatalf("note: %s", n)
	}
}
