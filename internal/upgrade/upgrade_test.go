package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/provenance"
)

const fixture = "../provenance/testdata/v0.24.0.sigstore.json"

// A release host serving whatever files the test gives it, under /releases/download/<tag>/.
func host(t *testing.T, tag string, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/releases/download/"+tag+"/")
		b, found := files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func options(srv *httptest.Server, latest string, self string) Options {
	return Options{
		Self:    self,
		Sources: []string{srv.URL},
		Client:  srv.Client(),
		GOOS:    "linux",
		Latest:  func(context.Context) (string, error) { return latest, nil },
	}
}

func TestUpgradeLeavesACheckedInstallerAndSaysHowToRunIt(t *testing.T) {
	script := []byte("#!/bin/sh\necho genuine\n")
	bundle := []byte(`{"stand-in":"bundle"}`)
	srv := host(t, "v0.25.0", map[string][]byte{"install.sh": script, Attestation: bundle})

	o := options(srv, "v0.25.0", "v0.24.0")
	o.Dir = t.TempDir()
	var asked struct {
		bundle []byte
		policy provenance.Policy
		name   string
		digest [32]byte
	}
	o.check = func(b []byte, p provenance.Policy, n string, d [32]byte) (*provenance.Result, error) {
		asked.bundle, asked.policy, asked.name, asked.digest = b, p, n, d
		return &provenance.Result{Tag: "v0.25.0", Commit: "abc", LogIndex: 7, SignedAt: time.Unix(0, 0)}, nil
	}

	var out bytes.Buffer
	if err := Run(context.Background(), &out, o); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(asked.bundle, bundle) || asked.name != "install.sh" || asked.digest != sha256.Sum256(script) {
		t.Errorf("checked the wrong thing: %q %q", asked.name, asked.bundle)
	}
	if asked.policy != provenance.ForTag("v0.25.0") {
		t.Errorf("checked against %+v, not release.yml on v0.25.0", asked.policy)
	}

	path := filepath.Join(o.Dir, "install.sh")
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, script) {
		t.Fatalf("the installer left behind is not the one checked: %q %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("installer mode %v", fi.Mode().Perm())
	}
	if want := "sudo sh " + path + " --upgrade --version v0.25.0"; !strings.Contains(out.String(), want) {
		t.Errorf("does not print %q:\n%s", want, out.String())
	}
}

// The real verifier, against the real v0.24.0 attestation: it does not attest an installer, so nothing an
// installer could be is accepted — which is also what every release before this change looks like.
func TestAnInstallerTheReleaseDidNotAttestIsNotUsed(t *testing.T) {
	bundle, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := host(t, "v0.24.0", map[string][]byte{"install.sh": []byte("#!/bin/sh\nrm -rf /\n"), Attestation: bundle})
	o := options(srv, "v0.24.0", "v0.23.0")
	o.Dir = t.TempDir()

	err = Run(context.Background(), &bytes.Buffer{}, o)
	if err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "install.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Error("an installer that failed the check was left behind")
	}
}

func TestNoAttestationNoUpgrade(t *testing.T) {
	srv := host(t, "v0.25.0", map[string][]byte{"install.sh": []byte("#!/bin/sh\n")})
	o := options(srv, "v0.25.0", "v0.24.0")
	o.Dir = t.TempDir()
	o.check = func([]byte, provenance.Policy, string, [32]byte) (*provenance.Result, error) {
		t.Fatal("checked with no attestation")
		return nil, nil
	}
	if err := Run(context.Background(), &bytes.Buffer{}, o); err == nil || !strings.Contains(err.Error(), Attestation) {
		t.Fatalf("got %v", err)
	}
}

func TestTheLatestIsNeverOlderThanThis(t *testing.T) {
	srv := host(t, "v0.20.0", nil)
	err := Run(context.Background(), &bytes.Buffer{}, options(srv, "v0.20.0", "v0.24.0"))
	if err == nil || !strings.Contains(err.Error(), "Not downgrading") {
		t.Fatalf("a 'latest' older than this agent was followed: %v", err)
	}

	var out bytes.Buffer
	if err := Run(context.Background(), &out, options(srv, "v0.24.0", "v0.24.0")); err != nil || !strings.Contains(out.String(), "already") {
		t.Fatalf("already the latest: %v %q", err, out.String())
	}
}

func TestAnOlderReleaseByNameIsAllowed(t *testing.T) {
	srv := host(t, "v0.20.0", map[string][]byte{"install.sh": []byte("x"), Attestation: []byte("{}")})
	o := options(srv, "v0.24.0", "v0.24.0")
	o.Version = "v0.20.0"
	o.Dir = t.TempDir()
	o.check = func([]byte, provenance.Policy, string, [32]byte) (*provenance.Result, error) {
		return &provenance.Result{Tag: "v0.20.0"}, nil
	}
	if err := Run(context.Background(), &bytes.Buffer{}, o); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyLinuxAndOnlyReleaseVersions(t *testing.T) {
	srv := host(t, "x", nil)
	o := options(srv, "v1.0.0", "v0.24.0")
	o.GOOS = "windows"
	if err := Run(context.Background(), &bytes.Buffer{}, o); err == nil || !strings.Contains(err.Error(), "install.ps1") {
		t.Errorf("windows: %v", err)
	}
	o = options(srv, "", "v0.24.0")
	o.Version = "../../etc"
	if err := Run(context.Background(), &bytes.Buffer{}, o); err == nil || !strings.Contains(err.Error(), "not a release version") {
		t.Errorf("a path as a version: %v", err)
	}
}

func TestVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	// A stand-in file cannot have the release's digest, so this checks the command's own plumbing: the
	// name it asks about, and that a refusal is an error saying which file.
	file := filepath.Join(dir, "infranest-agent_linux_amd64")
	os.WriteFile(file, []byte("not the release"), 0o644)

	err := Verify(&bytes.Buffer{}, VerifyOptions{File: file, Bundle: fixture, Self: "v0.24.0"})
	if err == nil || !strings.Contains(err.Error(), "infranest-agent_linux_amd64 does not verify") || !strings.Contains(err.Error(), "different SHA-256") {
		t.Fatalf("got %v", err)
	}
	if err := Verify(&bytes.Buffer{}, VerifyOptions{File: file}); err == nil {
		t.Error("no bundle was accepted")
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.9.1", "v0.24.0", true},
		{"v0.24.0", "v0.24.0", false},
		{"v1.0.0", "v0.99.99", false},
		{"v0.24.0", "dev", false},
		{"dev", "v0.24.0", false},
		{"v0.24.0-rc1", "v0.24.1", true},
	} {
		if got := older(c.a, c.b); got != c.want {
			t.Errorf("older(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
