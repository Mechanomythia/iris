package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// CA signs per-host leaf certs for TLS termination of injected hosts only.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// EnsureCA loads or creates a CA at certPath/keyPath (key mode 0600). The
// CA is name-constrained to hosts (the http identities): even if its key
// leaked, or it's trusted in the login keychain (iris ca trust), it can't
// impersonate any other site. A changed host list regenerates it
// (changed = true: trust it again and restart the broker).
func EnsureCA(certPath, keyPath string, hosts []string) (ca *CA, changed bool, err error) {
	want := normHosts(hosts)
	if c, err := LoadCA(certPath, keyPath); err == nil && sameHosts(c.cert.PermittedDNSDomains, want) && c.cert.PermittedDNSDomainsCritical {
		return c, false, nil
	}
	if err := generateCA(certPath, keyPath, want); err != nil {
		return nil, false, err
	}
	c, err := LoadCA(certPath, keyPath)
	return c, true, err
}

func normHosts(hosts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimPrefix(h, "*."))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = []string{"iris.invalid"} // no injected hosts: valid for nothing
	}
	return out
}

func sameHosts(a, b []string) bool {
	a = append([]string(nil), a...)
	sort.Strings(a)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// CommonNamePrefix identifies iris CAs (iris ca trust removes older ones).
const CommonNamePrefix = "Iris credential proxy CA"

func generateCA(certPath, keyPath string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: CommonNamePrefix + " (" + host + ")", Organization: []string{"Iris local"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// Valid only for the injected hosts (and their subdomains).
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func LoadCA(certPath, keyPath string) (*CA, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(cb, kb)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("CA key must be ECDSA")
	}
	return &CA{cert: cert, key: key, leafs: map[string]*tls.Certificate{}}, nil
}

// Hosts the CA may issue for.
func (ca *CA) Hosts() []string { return ca.cert.PermittedDNSDomains }

func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leafs[host]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-time.Hour)) {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	c := &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: leaf}
	ca.leafs[host] = c
	return c, nil
}
