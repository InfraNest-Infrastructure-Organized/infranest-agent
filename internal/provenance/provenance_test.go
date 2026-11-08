package provenance

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// The fixtures are real, unedited attestations:
//
//   - v0.24.0.sigstore.json is the one published with release v0.24.0, made by release.yml.
//   - signed-by-ci.sigstore.json was made by ci.yml on main, with the same action release.yml uses, for
//     v0.9.1's linux/amd64 binary with "signed by ci.yml" appended (CI's installer-sources job makes one
//     on every push). Everything about it is genuine except who signed it, which is what it is here for.
const (
	releaseFixture = "testdata/v0.24.0.sigstore.json"
	ciFixture      = "testdata/signed-by-ci.sigstore.json"
	releaseAsset   = "infranest-agent_linux_amd64"
	releaseDigest  = "ad708fffa6cee45fd6cd9d91ea1ceb254e824f1fc35f4eb57547a5dcdafe99b7"
	ciDigest       = "5cceccd6a4b48cd88d3e327d54ce15c40f104eec569f2d38339620791a5c4413"
)

func load(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func digestOf(t *testing.T, h string) [32]byte {
	t.Helper()
	var d [32]byte
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad digest %q", h)
	}
	copy(d[:], b)
	return d
}

func TestAGenuineReleaseVerifies(t *testing.T) {
	res, err := Verify(load(t, releaseFixture), Release, releaseAsset, digestOf(t, releaseDigest))
	if err != nil {
		t.Fatalf("a genuine release was refused: %v", err)
	}
	if res.Tag != "v0.24.0" || res.Ref != "refs/tags/v0.24.0" {
		t.Errorf("tag %q ref %q", res.Tag, res.Ref)
	}
	if res.Commit != "2e3e9c9c491df518a2f3acf5f8927ca230307754" {
		t.Errorf("commit %q", res.Commit)
	}
	if res.LogIndex != 3746718708 || res.SignedAt != time.Unix(1794089023, 0).UTC() {
		t.Errorf("log index %d at %s", res.LogIndex, res.SignedAt)
	}

	if _, err := Verify(load(t, releaseFixture), ForTag("v0.24.0"), releaseAsset, digestOf(t, releaseDigest)); err != nil {
		t.Errorf("pinned to its own tag: %v", err)
	}
}

// Every file the release attests verifies by its own name and digest, so the installer on any
// architecture gets the same answer.
func TestEveryAttestedFileVerifies(t *testing.T) {
	raw := load(t, releaseFixture)
	for _, s := range subjects(t, raw) {
		if _, err := Verify(raw, Release, s.name, digestOf(t, s.digest)); err != nil {
			t.Errorf("%s: %v", s.name, err)
		}
	}
}

func TestAnotherReleaseIsNotThisOne(t *testing.T) {
	_, err := Verify(load(t, releaseFixture), ForTag("v0.23.2"), releaseAsset, digestOf(t, releaseDigest))
	if !errors.Is(err, errNotOurReleaseBuild) {
		t.Fatalf("a bundle for v0.24.0 was accepted for v0.23.2: %v", err)
	}
}

func TestADifferentBinaryIsRefused(t *testing.T) {
	d := digestOf(t, releaseDigest)
	d[0] ^= 1
	if _, err := Verify(load(t, releaseFixture), Release, releaseAsset, d); !errors.Is(err, errNotThisArtefact) {
		t.Errorf("a binary with another digest: %v", err)
	}
	// The genuine amd64 bytes, offered as another architecture's file.
	if _, err := Verify(load(t, releaseFixture), Release, "infranest-agent_linux_arm64", digestOf(t, releaseDigest)); !errors.Is(err, errNotThisArtefact) {
		t.Errorf("the right bytes under the wrong name: %v", err)
	}
	if _, err := Verify(load(t, releaseFixture), Release, "infranest-agent_freebsd_amd64", digestOf(t, releaseDigest)); !errors.Is(err, errNotThisArtefact) {
		t.Errorf("a file the release does not have: %v", err)
	}
}

// The pin, tried rather than read: a properly signed, properly logged attestation from a workflow that
// is not release.yml is refused — and the same bundle under a policy naming ci.yml is accepted, so the
// refusal is the identity check and not the bundle being unreadable.
func TestSignedByAnotherWorkflowIsRefused(t *testing.T) {
	raw := load(t, ciFixture)
	_, err := Verify(raw, Release, releaseAsset, digestOf(t, ciDigest))
	if !errors.Is(err, errNotOurReleaseBuild) {
		t.Fatalf("an attestation from ci.yml was not refused by identity: %v", err)
	}
	if !strings.Contains(err.Error(), "ci.yml@refs/heads/main") {
		t.Errorf("the refusal does not say who signed it: %v", err)
	}

	control := Release
	control.Workflow = ".github/workflows/ci.yml"
	control.Ref = "refs/heads/main"
	if _, err := Verify(raw, control, releaseAsset, digestOf(t, ciDigest)); err != nil {
		t.Fatalf("control: the same bundle pinned to ci.yml should verify: %v", err)
	}
}

func TestARepositoryWithOnlyTheSameNameIsRefused(t *testing.T) {
	for _, p := range []Policy{
		func() Policy { p := Release; p.RepositoryID = "1"; return p }(),
		func() Policy { p := Release; p.OwnerID = "1"; return p }(),
		func() Policy { p := Release; p.Repository = "someone-else/infranest-agent"; return p }(),
	} {
		if _, err := Verify(load(t, releaseFixture), p, releaseAsset, digestOf(t, releaseDigest)); !errors.Is(err, errNotOurReleaseBuild) {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

// Every part of the bundle that the verdict depends on, changed one at a time. Each must be refused —
// and none of them for being unparseable, which would make the test pass for the wrong reason.
func TestTamperingIsRefused(t *testing.T) {
	ci := decode(t, load(t, ciFixture))

	cases := map[string]func(b map[string]any){
		"payload changed": func(b map[string]any) {
			env := b["dsseEnvelope"].(map[string]any)
			p := mustB64(t, env["payload"])
			env["payload"] = b64(bytes.Replace(p, []byte(releaseDigest), []byte(strings.Repeat("0", 64)), 1))
		},
		"signature changed": func(b map[string]any) {
			s := b["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)
			sig := mustB64(t, s["sig"])
			sig[len(sig)-1] ^= 1
			s["sig"] = b64(sig)
		},
		"another workflow's certificate": func(b map[string]any) {
			b["verificationMaterial"].(map[string]any)["certificate"] = ci["verificationMaterial"].(map[string]any)["certificate"]
		},
		"another signature's log entry": func(b map[string]any) {
			b["verificationMaterial"].(map[string]any)["tlogEntries"] = ci["verificationMaterial"].(map[string]any)["tlogEntries"]
		},
		"integrated time moved": func(b map[string]any) {
			tlog(b)["integratedTime"] = "1794089999"
		},
		"log index moved": func(b map[string]any) {
			tlog(b)["logIndex"] = "3746718709"
		},
		"signed entry timestamp changed": func(b map[string]any) {
			p := tlog(b)["inclusionPromise"].(map[string]any)
			s := mustB64(t, p["signedEntryTimestamp"])
			s[len(s)-1] ^= 1
			p["signedEntryTimestamp"] = b64(s)
		},
		"inclusion proof hash changed": func(b map[string]any) {
			hs := tlog(b)["inclusionProof"].(map[string]any)["hashes"].([]any)
			h := mustB64(t, hs[3])
			h[0] ^= 1
			hs[3] = b64(h)
		},
		"inclusion proof shortened": func(b map[string]any) {
			p := tlog(b)["inclusionProof"].(map[string]any)
			hs := p["hashes"].([]any)
			p["hashes"] = hs[:len(hs)-1]
		},
		"checkpoint signature changed": func(b map[string]any) {
			p := tlog(b)["inclusionProof"].(map[string]any)["checkpoint"].(map[string]any)
			env := p["envelope"].(string)
			i := strings.LastIndex(env, " ")
			sig := mustB64(t, strings.TrimSpace(env[i+1:]))
			sig[len(sig)-1] ^= 1
			p["envelope"] = env[:i+1] + base64.StdEncoding.EncodeToString(sig) + "\n"
		},
		"checkpoint for another tree": func(b map[string]any) {
			p := tlog(b)["inclusionProof"].(map[string]any)["checkpoint"].(map[string]any)
			p["envelope"] = ci["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)[0].(map[string]any)["inclusionProof"].(map[string]any)["checkpoint"].(map[string]any)["envelope"]
		},
		"log entry body changed": func(b map[string]any) {
			body := mustB64(t, tlog(b)["canonicalizedBody"])
			tlog(b)["canonicalizedBody"] = b64(bytes.Replace(body, []byte(`"sha256"`), []byte(`"sha512"`), 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := decode(t, load(t, releaseFixture))
			mutate(b)
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Verify(raw, Release, releaseAsset, digestOf(t, releaseDigest))
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, errUnsupportedFormat) || strings.Contains(err.Error(), "not readable") {
				t.Fatalf("refused, but as unreadable rather than as tampered: %v", err)
			}
		})
	}
}

// Shapes we do not publish are refused as such, never half-checked.
func TestShapesWeDoNotPublishAreRefused(t *testing.T) {
	cases := map[string]func(b map[string]any){
		"an older bundle format": func(b map[string]any) {
			b["mediaType"] = "application/vnd.dev.sigstore.bundle+json;version=0.2"
		},
		"no inclusion promise (a Rekor v2 entry has none)": func(b map[string]any) {
			delete(tlog(b), "inclusionPromise")
		},
		"no inclusion proof": func(b map[string]any) {
			delete(tlog(b), "inclusionProof")
		},
		"two log entries": func(b map[string]any) {
			vm := b["verificationMaterial"].(map[string]any)
			es := vm["tlogEntries"].([]any)
			vm["tlogEntries"] = append(es, es[0])
		},
		"no log entry": func(b map[string]any) {
			b["verificationMaterial"].(map[string]any)["tlogEntries"] = []any{}
		},
		"a log entry of another kind": func(b map[string]any) {
			tlog(b)["kindVersion"] = map[string]any{"kind": "dsse", "version": "0.0.2"}
		},
		"two signatures": func(b map[string]any) {
			env := b["dsseEnvelope"].(map[string]any)
			s := env["signatures"].([]any)
			env["signatures"] = append(s, s[0])
		},
		"no certificate": func(b map[string]any) {
			delete(b["verificationMaterial"].(map[string]any), "certificate")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := decode(t, load(t, releaseFixture))
			mutate(b)
			raw, _ := json.Marshal(b)
			if _, err := Verify(raw, Release, releaseAsset, digestOf(t, releaseDigest)); !errors.Is(err, errUnsupportedFormat) {
				t.Fatalf("got %v", err)
			}
		})
	}

	if _, err := Verify([]byte("<html>not found</html>"), Release, releaseAsset, digestOf(t, releaseDigest)); err == nil {
		t.Error("an HTML page was accepted as an attestation")
	}
}

func TestACertificateWithoutTransparencyIsRefused(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	if err := verifySCT(c, c); err == nil {
		t.Fatal("a certificate with no SCT was accepted")
	}
}

// The Merkle proof check against RFC 6962's own recursive definitions, for every leaf of every tree up to
// a size where all the border cases have occurred.
func TestInclusionProofsAgainstTheRFC(t *testing.T) {
	for n := 1; n <= 33; n++ {
		leaves := make([][]byte, n)
		for i := range leaves {
			leaves[i] = []byte{byte(i), byte(n)}
		}
		root := mth(leaves)
		for i := 0; i < n; i++ {
			lh := sha256.Sum256(append([]byte{0}, leaves[i]...))
			got, err := rootFromInclusionProof(uint64(i), uint64(n), lh[:], path(i, leaves))
			if err != nil || !bytes.Equal(got, root) {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if n > 1 {
				p := path(i, leaves)
				p[0] = make([]byte, 32)
				if got, _ := rootFromInclusionProof(uint64(i), uint64(n), lh[:], p); bytes.Equal(got, root) {
					t.Fatalf("n=%d i=%d: a wrong proof reached the root", n, i)
				}
			}
		}
	}
}

func mth(d [][]byte) []byte {
	if len(d) == 1 {
		h := sha256.Sum256(append([]byte{0}, d[0]...))
		return h[:]
	}
	k := split(len(d))
	return hashChildren(mth(d[:k]), mth(d[k:]))
}

func path(m int, d [][]byte) [][]byte {
	if len(d) == 1 {
		return nil
	}
	k := split(len(d))
	if m < k {
		return append(path(m, d[:k]), mth(d[k:]))
	}
	return append(path(m-k, d[k:]), mth(d[:k]))
}

func split(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

type subject struct{ name, digest string }

func subjects(t *testing.T, raw []byte) []subject {
	t.Helper()
	b := decode(t, raw)
	var st struct {
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(mustB64(t, b["dsseEnvelope"].(map[string]any)["payload"]), &st); err != nil {
		t.Fatal(err)
	}
	var out []subject
	for _, s := range st.Subject {
		out = append(out, subject{s.Name, s.Digest["sha256"]})
	}
	if len(out) < 9 {
		t.Fatalf("only %d subjects", len(out))
	}
	return out
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func tlog(b map[string]any) map[string]any {
	return b["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)[0].(map[string]any)
}

func mustB64(t *testing.T, v any) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(v.(string))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
