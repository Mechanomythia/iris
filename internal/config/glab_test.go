package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Configs written before 0.2.7 get the glab refresh without re-provisioning.
func TestOldGlabIdentityGetsRefresh(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`identities:
  - tool: http
    target: git.example.org
    source:
      command: [glab, config, get, token, --host, git.example.org]
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Identities[0]
	if !reflect.DeepEqual(id.Refresh, []string{"glab", "api", "--hostname", "git.example.org", "user"}) || id.RejectHint == "" {
		t.Fatalf("%+v", id)
	}
}
