package collect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A certificate for the given names, expiring at notAfter, as PEM.
func testCert(t *testing.T, notAfter time.Time, names ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeFixture(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

const webrootConf = `# renew_before_expiry = 30 days
version = 2.11.0
archive_dir = /etc/letsencrypt/archive/example.com
cert = /etc/letsencrypt/live/example.com/cert.pem
privkey = /etc/letsencrypt/live/example.com/privkey.pem

# Options used in the renewal process
[renewalparams]
account = 0123456789abcdef
authenticator = webroot
server = https://acme-v02.api.letsencrypt.org/directory
key_type = ecdsa
[[webroot_map]]
example.com = /var/www/html
www.example.com = /var/www/html
`

const nginxConf = `cert = /etc/letsencrypt/live/shop.example.org-0001/cert.pem
[renewalparams]
authenticator = nginx
installer = nginx
`

func TestCertbotNamesComeFromTheWebrootMap(t *testing.T) {
	r := parseCertbotRenewal("example.com", strings.NewReader(webrootConf))

	if r.Tool != "certbot" || r.Name != "example.com" || r.Authenticator != "webroot" {
		t.Fatalf("unexpected renewer: %+v", r.Renewer)
	}
	if !reflect.DeepEqual(r.Domains, []string{"example.com", "www.example.com"}) || r.DomainsGuessed {
		t.Fatalf("domains = %v guessed=%v", r.Domains, r.DomainsGuessed)
	}
	if r.certPath != "/etc/letsencrypt/live/example.com/cert.pem" {
		t.Fatalf("cert path = %q", r.certPath)
	}
}

// Without a webroot map the config does not list names; the lineage is the first name, and says so.
func TestCertbotFallsBackToTheLineageAndSaysItGuessed(t *testing.T) {
	r := parseCertbotRenewal("shop.example.org-0001", strings.NewReader(nginxConf))

	if !reflect.DeepEqual(r.Domains, []string{"shop.example.org"}) || !r.DomainsGuessed {
		t.Fatalf("domains = %v guessed=%v", r.Domains, r.DomainsGuessed)
	}
	if r.Authenticator != "nginx" {
		t.Fatalf("authenticator = %q", r.Authenticator)
	}
}

// When the certificate itself is readable, its names and expiry win over the config's.
func TestCertbotReadsTheCertificateWhenItMay(t *testing.T) {
	root := t.TempDir()
	expires := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	writeFixture(t, filepath.Join(root, certbotRenewalDir, "shop.example.org-0001.conf"), []byte(nginxConf), 0o644)
	writeFixture(t, filepath.Join(root, "/etc/letsencrypt/live/shop.example.org-0001/cert.pem"), testCert(t, expires, "shop.example.org", "api.example.org"), 0o644)

	got := readRenewers(root)

	if len(got) != 1 {
		t.Fatalf("want one renewer, got %+v", got)
	}
	if !reflect.DeepEqual(got[0].Domains, []string{"shop.example.org", "api.example.org"}) || got[0].DomainsGuessed {
		t.Fatalf("domains = %v guessed=%v", got[0].Domains, got[0].DomainsGuessed)
	}
	if got[0].NotAfter == nil || !got[0].NotAfter.Equal(expires) {
		t.Fatalf("not_after = %v", got[0].NotAfter)
	}
	if got[0].Source != "/etc/letsencrypt/renewal/shop.example.org-0001.conf" {
		t.Fatalf("source = %q (the test root must not leak into it)", got[0].Source)
	}
}

func TestATimerRunIsEveryCertbotCertificatesRun(t *testing.T) {
	last := time.Date(2026, 10, 5, 3, 12, 0, 0, time.UTC)
	next := last.Add(12 * time.Hour)
	renewers := []Renewer{{Tool: "certbot", Name: "a"}, {Tool: "certbot", Name: "b"}, {Tool: "traefik", Unreadable: true}}

	applyCertbotRun(renewers, certbotRun{unit: "certbot.timer", last: &last, next: &next, result: "exit-code"})

	for _, r := range renewers[:2] {
		if r.Unit != "certbot.timer" || r.LastResult != "exit-code" || !r.LastRunAt.Equal(last) || !r.NextRunAt.Equal(next) {
			t.Fatalf("run not applied: %+v", r)
		}
	}
	if renewers[2].Unit != "" {
		t.Fatalf("another tool got certbot's run: %+v", renewers[2])
	}
}

func TestCaddyReadsItsCertificateOrItsMetadata(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, caddyStores[0], "acme-v02.api.letsencrypt.org-directory")
	expires := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	writeFixture(t, filepath.Join(store, "example.net", "example.net.crt"), testCert(t, expires, "example.net"), 0o644)
	meta, _ := json.Marshal(map[string]any{"sans": []string{"blog.example.net"}, "issuer_data": map[string]any{}})
	writeFixture(t, filepath.Join(store, "blog.example.net", "blog.example.net.json"), meta, 0o644)

	got := readRenewers(root)

	if len(got) != 2 {
		t.Fatalf("want two, got %+v", got)
	}
	byName := map[string]Renewer{got[0].Name: got[0], got[1].Name: got[1]}
	if r := byName["example.net"]; r.Tool != "caddy" || r.NotAfter == nil || !r.NotAfter.Equal(expires) {
		t.Fatalf("certificate not read: %+v", r)
	}
	if r := byName["blog.example.net"]; !reflect.DeepEqual(r.Domains, []string{"blog.example.net"}) || r.NotAfter != nil {
		t.Fatalf("metadata not read: %+v", r)
	}
}

func TestTraefikDecodesOnlyThePublicCertificate(t *testing.T) {
	expires := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	store, _ := json.Marshal(map[string]any{
		"letsencrypt": map[string]any{
			"Account": map[string]any{"Email": "ops@example.com", "PrivateKey": "c2VjcmV0"},
			"Certificates": []map[string]any{{
				"domain":      map[string]any{"main": "app.example.com", "sans": []string{"www.app.example.com"}},
				"certificate": base64.StdEncoding.EncodeToString(testCert(t, expires, "app.example.com", "www.app.example.com")),
				"key":         "c2VjcmV0",
			}},
		},
	})

	got := parseTraefikStore(strings.NewReader(string(store)))

	if len(got) != 1 || got[0].Name != "app.example.com" || got[0].NotAfter == nil || !got[0].NotAfter.Equal(expires) {
		t.Fatalf("unexpected: %+v", got)
	}
	if !reflect.DeepEqual(got[0].Domains, []string{"app.example.com", "www.app.example.com"}) {
		t.Fatalf("domains = %v", got[0].Domains)
	}
	if out, _ := json.Marshal(got); strings.Contains(string(out), "c2VjcmV0") || strings.Contains(string(out), "ops@example.com") {
		t.Fatalf("the key or the account left the machine: %s", out)
	}
}

// A store that refused us is a fact worth sending; a tool that is not installed is not.
func TestARefusedStoreIsReportedAndAMissingOneIsNot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads owner-only files; run unprivileged to exercise the refusal")
	}
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, traefikStores[0]), []byte(`{}`), 0o600)
	if err := os.Chmod(filepath.Join(root, traefikStores[0]), 0o000); err != nil {
		t.Fatal(err)
	}

	got := readRenewers(root)

	if len(got) != 1 || got[0].Tool != "traefik" || !got[0].Unreadable || got[0].Source != traefikStores[0] || got[0].Mode != "0" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestNothingInstalledIsAnEmptyList(t *testing.T) {
	if got := readRenewers(t.TempDir()); len(got) != 0 {
		t.Fatalf("want none, got %+v", got)
	}
}
