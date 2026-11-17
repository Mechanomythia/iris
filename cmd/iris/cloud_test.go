package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudWizardNoCreate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("IRIS_HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin") // no gcloud: project has no default
	var clip []string
	old := copyClip
	copyClip = func(s string) bool { clip = append(clip, s); return true }
	t.Cleanup(func() { copyClip = old })

	answers := strings.Join([]string{
		"acme-api",                                 // name
		"1,2,6,8",                                  // GitHub, GCP, AWS, APIs
		"acme-prod",                                // GCP project
		"write",                                    // GCP access
		"",                                         // roles default
		"",                                         // AWS region default
		"r",                                        // AWS access
		"",                                         // policies default
		"1,9",                                      // OpenAI + Other
		"Internal",                                 // other name
		"api.acme.dev",                             // host
		"X-Api-Key",                                // header
		"",                                         // prefix -> default Bearer; then we test the empty-prefix path via Linear elsewhere
		"y",                                        // walkthrough
		"", "", "", "", "", "", "", "", "", "", "", // Enter presses
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := cloudNew([]string{"--no-create"}, strings.NewReader(answers), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	o := out.String()
	for _, w := range []string{"What does Claude need?", "Saved to", "Network secrets", "First session", "iris cloud show acme-api"} {
		if !strings.Contains(o, w) {
			t.Errorf("output missing %q", w)
		}
	}
	dir := filepath.Join(home, ".config", "iris", "cloud", "acme-api")
	for _, f := range []string{"guide.md", "setup.sh", "request.json", "secrets.json"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, st)
		}
	}
	guide, _ := os.ReadFile(filepath.Join(dir, "guide.md"))
	for _, w := range []string{"roles/editor", "AWS_ACCESS_KEY_ID=<access key id, from iris>", "`api.acme.dev` | `X-Api-Key`", "api.openai.com"} {
		if !strings.Contains(string(guide)+string(mustRead(t, filepath.Join(dir, "request.json"))), w) {
			t.Errorf("guide/request missing %q", w)
		}
	}
	all := strings.Join(clip, "\n---\n")
	if !strings.Contains(all, "acme-api") || !strings.Contains(all, "packages.cloud.google.com") || !strings.Contains(all, "#!/bin/bash") || !strings.Contains(all, "api.openai.com") {
		t.Errorf("clipboard sequence incomplete:\n%s", all)
	}

	// show --print re-renders from the saved files.
	var shown bytes.Buffer
	if err := cloudShow([]string{"--print", "acme-api"}, strings.NewReader(""), &shown); err != nil || !strings.Contains(shown.String(), "# Claude Code cloud environment: acme-api") {
		t.Fatalf("%v %s", err, shown.String())
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
