package proxy

import (
	"crypto/x509"
	"path/filepath"
	"testing"
)

func TestCAIsNameConstrained(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	ca, changed, err := EnsureCA(cp, kp, []string{"gitlab.com"})
	if err != nil || !changed {
		t.Fatal(err, changed)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	verify := func(host string) error {
		leaf, err := ca.Leaf(host)
		if err != nil {
			return err
		}
		_, err = leaf.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots})
		return err
	}
	if err := verify("gitlab.com"); err != nil {
		t.Fatalf("gitlab.com should verify: %v", err)
	}
	if err := verify("api.gitlab.com"); err != nil {
		t.Fatalf("subdomain should verify: %v", err)
	}
	for _, evil := range []string{"bank.example.com", "github.com", "gitlab.com.evil.net"} {
		if verify(evil) == nil {
			t.Fatalf("CA vouched for %s despite name constraints", evil)
		}
	}
	// Same hosts: kept. New hosts: regenerated.
	if _, changed, _ := EnsureCA(cp, kp, []string{"gitlab.com"}); changed {
		t.Fatal("unchanged hosts regenerated the CA")
	}
	ca2, changed, _ := EnsureCA(cp, kp, []string{"gitlab.com", "api.openai.com"})
	if !changed || len(ca2.Hosts()) != 2 {
		t.Fatal("new host list must regenerate", ca2.Hosts())
	}
	// No hosts: valid for nothing real.
	ca3, _, _ := EnsureCA(cp, kp, nil)
	if len(ca3.Hosts()) != 1 || ca3.Hosts()[0] != "iris.invalid" {
		t.Fatal(ca3.Hosts())
	}
}
