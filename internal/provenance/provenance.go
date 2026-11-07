/*
Package provenance checks that a release binary was built by this repository's release workflow, with
nothing but the standard library and the trust anchors compiled into this binary (InfraNest #5969).

Every release carries a build attestation (`infranest-agent.sigstore.json`): a Sigstore bundle holding an
in-toto statement that names each artefact by its SHA-256, signed with a short-lived certificate that
Fulcio issued to release.yml, and recorded in Rekor, the public transparency log. `gh attestation verify`
and `cosign` check it — but almost no server has either, so for most of them the only check was a
checksum served from the same place as the binary. This package is the same check, done by the agent
already installed, so an upgrade trusts what is on the machine rather than whatever served the download.

What is verified, and why each step is there:

  - The certificate chains to the Fulcio CA, at the moment Rekor recorded the signature. Fulcio
    certificates live ten minutes; Rekor's signed timestamp is what proves the signature was made inside
    them.
  - The certificate was logged in certificate transparency (its SCT verifies against Sigstore's CT log),
    so a certificate issued for this workflow cannot exist without being publicly visible.
  - The certificate names exactly this workflow, in this repository — by name *and* by GitHub's numeric
    repository and owner ids, so a renamed or re-created repository of the same name does not inherit the
    trust — on a release tag, on a GitHub-hosted runner, issued against GitHub's OIDC issuer. Any workflow
    in this repository can get a certificate; only release.yml builds releases.
  - The DSSE signature over the statement verifies with that certificate's key.
  - Rekor's entry is this signature by this certificate: its signed entry timestamp verifies, its Merkle
    inclusion proof leads to a checkpoint Rekor signed, and the entry's body names this payload, this
    signature and this certificate.
  - The statement is SLSA provenance and lists the artefact by its exact file name and SHA-256.

What it does not do, deliberately: fetch anything. There is no TUF client and no online Rekor lookup —
everything needed is in the bundle and in trustroot.go. A bundle in a format this code does not know
(a Rekor v2 entry, an RFC 3161 timestamp instead of Rekor's) is refused rather than half-checked, and
release.yml runs the previous release's verifier against every new bundle so that day is found before a
release ships.
*/
package provenance

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Policy is who must have signed. Release is the only one the agent uses; tests build others to prove a
// refusal comes from the identity and not from an unreadable bundle.
type Policy struct {
	// Repository is "owner/name"; RepositoryID and OwnerID are GitHub's numeric ids for the same thing,
	// which survive a rename — and which a different repository that later takes the name does not have.
	Repository   string
	RepositoryID string
	OwnerID      string
	// Workflow is the path of the one workflow file allowed to sign.
	Workflow string
	// Ref, when set, is the exact git ref the workflow must have run on. Left empty, any release tag
	// (refs/tags/v…) is accepted.
	Ref string
}

// Release is this repository's release workflow, on a release tag.
//
// The ids are GitHub's for InfraNest-Infrastructure-Organized/infranest-agent and its owner, as every
// release certificate carries them (Fulcio extensions 1.3.6.1.4.1.57264.1.15 and .17).
var Release = Policy{
	Repository:   "InfraNest-Infrastructure-Organized/infranest-agent",
	RepositoryID: "1344945523",
	OwnerID:      "295990078",
	Workflow:     ".github/workflows/release.yml",
}

// ForTag is the release policy narrowed to one tag, so a bundle for another release — genuine, but not
// the one asked for — is refused too.
func ForTag(tag string) Policy {
	p := Release
	p.Ref = "refs/tags/" + tag
	return p
}

// Result is what a verified bundle says about the artefact.
type Result struct {
	// Ref is the git ref the release workflow ran on, e.g. refs/tags/v0.24.0, and Tag the tag in it.
	Ref string
	Tag string
	// Commit is the commit it built.
	Commit string
	// SignedAt is when Rekor recorded the signature; LogIndex is where, so anyone can look it up.
	SignedAt time.Time
	LogIndex int64
}

const (
	oidcIssuer        = "https://token.actions.githubusercontent.com"
	inTotoPayloadType = "application/vnd.in-toto+json"
	statementType     = "https://in-toto.io/Statement/v1"
	slsaProvenanceV1  = "https://slsa.dev/provenance/v1"
	maxBundleSize     = 1 << 20
)

// Fulcio's certificate extensions (https://github.com/sigstore/fulcio/blob/main/docs/oid-info.md). The
// ones from .8 on are DER-encoded strings; .1 is the older raw form of the issuer.
var (
	oidIssuerV1           = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidIssuer             = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidBuildSignerURI     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	oidRunnerEnvironment  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 11}
	oidSourceRepoURI      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}
	oidSourceRepoDigest   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}
	oidSourceRepoRef      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}
	oidSourceRepoID       = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 15}
	oidSourceRepoOwnerID  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 17}
	oidSCTList            = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
	errNotThisArtefact    = errors.New("the attestation does not cover this file")
	errUnsupportedFormat  = errors.New("the attestation is in a format this agent cannot check")
	errNotOurReleaseBuild = errors.New("the attestation was not made by this repository's release workflow")
)

// Verify checks bundleJSON for the artefact called name whose SHA-256 is digest, against policy. Any
// error means: do not install it.
func Verify(bundleJSON []byte, policy Policy, name string, digest [32]byte) (*Result, error) {
	if len(bundleJSON) > maxBundleSize {
		return nil, fmt.Errorf("%w: it is larger than any attestation we publish", errUnsupportedFormat)
	}

	var b bundle
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return nil, fmt.Errorf("the attestation is not readable: %w", err)
	}
	if err := b.check(); err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(b.VerificationMaterial.Certificate.RawBytes)
	if err != nil {
		return nil, fmt.Errorf("the signing certificate is not readable: %w", err)
	}
	entry := b.VerificationMaterial.TlogEntries[0]
	env := b.DSSEEnvelope
	sig := env.Signatures[0].Sig

	// The transparency log first: it is what fixes the time everything else is judged at.
	signedAt, err := verifyTlogEntry(entry, env.Payload, sig, leaf.Raw)
	if err != nil {
		return nil, err
	}

	issuer, err := verifyCertificate(leaf, signedAt)
	if err != nil {
		return nil, err
	}
	if err := verifySCT(leaf, issuer); err != nil {
		return nil, err
	}

	res, err := checkIdentity(leaf, policy)
	if err != nil {
		return nil, err
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: the signing key is not ECDSA", errUnsupportedFormat)
	}
	if !ecdsa.VerifyASN1(pub, hashFor(pub, pae(env.PayloadType, env.Payload)), sig) {
		return nil, errors.New("the attestation's signature does not verify")
	}

	if err := checkStatement(env.PayloadType, env.Payload, name, digest); err != nil {
		return nil, err
	}

	res.SignedAt = signedAt
	res.LogIndex = int64(entry.LogIndex)

	return res, nil
}

// bundle is the part of a v0.3 Sigstore bundle (protobuf JSON) that a GitHub artifact attestation uses.
type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate *struct {
			RawBytes []byte `json:"rawBytes"`
		} `json:"certificate"`
		TlogEntries []tlogEntry `json:"tlogEntries"`
	} `json:"verificationMaterial"`
	DSSEEnvelope *struct {
		Payload     []byte `json:"payload"`
		PayloadType string `json:"payloadType"`
		Signatures  []struct {
			Sig []byte `json:"sig"`
		} `json:"signatures"`
	} `json:"dsseEnvelope"`
}

type tlogEntry struct {
	LogIndex jsonInt64 `json:"logIndex"`
	LogID    struct {
		KeyID []byte `json:"keyId"`
	} `json:"logId"`
	KindVersion struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
	} `json:"kindVersion"`
	IntegratedTime   jsonInt64 `json:"integratedTime"`
	InclusionPromise *struct {
		SignedEntryTimestamp []byte `json:"signedEntryTimestamp"`
	} `json:"inclusionPromise"`
	InclusionProof *struct {
		LogIndex   jsonInt64 `json:"logIndex"`
		RootHash   []byte    `json:"rootHash"`
		TreeSize   jsonInt64 `json:"treeSize"`
		Hashes     [][]byte  `json:"hashes"`
		Checkpoint struct {
			Envelope string `json:"envelope"`
		} `json:"checkpoint"`
	} `json:"inclusionProof"`
	CanonicalizedBody []byte `json:"canonicalizedBody"`
}

// check refuses any shape but the one release.yml produces, before any of it is trusted. Exactly one of
// each: a bundle with a second signature or a second log entry is not something we publish, and
// accepting "at least one good one" is how a verifier ends up checking one thing and reporting on another.
func (b *bundle) check() error {
	switch b.MediaType {
	case "application/vnd.dev.sigstore.bundle.v0.3+json", "application/vnd.dev.sigstore.bundle+json;version=0.3":
	default:
		return fmt.Errorf("%w: bundle type %q", errUnsupportedFormat, b.MediaType)
	}
	vm := b.VerificationMaterial
	if vm.Certificate == nil || len(vm.Certificate.RawBytes) == 0 {
		return fmt.Errorf("%w: it carries no signing certificate", errUnsupportedFormat)
	}
	if b.DSSEEnvelope == nil || len(b.DSSEEnvelope.Signatures) != 1 || len(b.DSSEEnvelope.Signatures[0].Sig) == 0 {
		return fmt.Errorf("%w: it is not a singly-signed DSSE envelope", errUnsupportedFormat)
	}
	if len(vm.TlogEntries) != 1 {
		return fmt.Errorf("%w: it has %d transparency-log entries, not one", errUnsupportedFormat, len(vm.TlogEntries))
	}
	e := vm.TlogEntries[0]
	if e.KindVersion.Kind != "dsse" || e.KindVersion.Version != "0.0.1" {
		return fmt.Errorf("%w: a %s/%s log entry", errUnsupportedFormat, e.KindVersion.Kind, e.KindVersion.Version)
	}
	if e.InclusionPromise == nil || e.InclusionProof == nil {
		return fmt.Errorf("%w: the log entry lacks its signed timestamp or its inclusion proof", errUnsupportedFormat)
	}
	return nil
}

// verifyCertificate chains leaf to the embedded Fulcio CA as of signedAt, and returns the certificate
// that issued it.
func verifyCertificate(leaf *x509.Certificate, signedAt time.Time) (*x509.Certificate, error) {
	if signedAt.Before(fulcioValidFrom) {
		return nil, errors.New("the signature predates the certificate authority it claims")
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	for i, s := range fulcioChain {
		der, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		if i == len(fulcioChain)-1 {
			roots.AddCert(c)
		} else {
			intermediates.AddCert(c)
		}
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   signedAt,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	})
	if err != nil {
		return nil, fmt.Errorf("the signing certificate was not issued by Sigstore's certificate authority, or had expired when it was used: %w", err)
	}
	return chains[0][1], nil
}

// checkIdentity holds the certificate to policy, and reads the ref and commit from it.
func checkIdentity(leaf *x509.Certificate, p Policy) (*Result, error) {
	ext := func(oid asn1.ObjectIdentifier) string {
		for _, e := range leaf.Extensions {
			if e.Id.Equal(oid) {
				var s string
				if _, err := asn1.Unmarshal(e.Value, &s); err == nil {
					return s
				}
				// The issuer's older extension (.1) holds the string itself, not an encoded one.
				if oid.Equal(oidIssuerV1) {
					return string(e.Value)
				}
			}
		}
		return ""
	}

	issuer := ext(oidIssuer)
	if issuer == "" {
		issuer = ext(oidIssuerV1)
	}
	if issuer != oidcIssuer {
		return nil, fmt.Errorf("%w: the certificate was issued to %q, not GitHub Actions", errNotOurReleaseBuild, issuer)
	}

	repoURL := "https://github.com/" + p.Repository
	ref := ext(oidSourceRepoRef)
	identity := repoURL + "/" + p.Workflow + "@" + ref
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != identity ||
		len(leaf.DNSNames)+len(leaf.EmailAddresses)+len(leaf.IPAddresses) != 0 {
		return nil, fmt.Errorf("%w: it was signed by %s", errNotOurReleaseBuild, describeSigner(leaf))
	}
	if ext(oidBuildSignerURI) != identity || ext(oidSourceRepoURI) != repoURL {
		return nil, fmt.Errorf("%w: the certificate's build and source extensions do not name it", errNotOurReleaseBuild)
	}
	if ext(oidSourceRepoID) != p.RepositoryID || ext(oidSourceRepoOwnerID) != p.OwnerID {
		return nil, fmt.Errorf("%w: it was built in a repository that only has the same name", errNotOurReleaseBuild)
	}
	if env := ext(oidRunnerEnvironment); env != "github-hosted" {
		return nil, fmt.Errorf("%w: it was built on a %q runner, not a GitHub-hosted one", errNotOurReleaseBuild, env)
	}

	tag := strings.TrimPrefix(ref, "refs/tags/")
	switch {
	case p.Ref != "" && ref != p.Ref:
		return nil, fmt.Errorf("%w: it was built for %s, not %s", errNotOurReleaseBuild, ref, p.Ref)
	case p.Ref == "" && (tag == ref || !strings.HasPrefix(tag, "v")):
		return nil, fmt.Errorf("%w: it was not built from a release tag (%s)", errNotOurReleaseBuild, ref)
	}
	if tag == ref {
		tag = ""
	}

	return &Result{Ref: ref, Tag: tag, Commit: ext(oidSourceRepoDigest)}, nil
}

func describeSigner(leaf *x509.Certificate) string {
	if len(leaf.URIs) == 1 {
		return leaf.URIs[0].String()
	}
	return "an identity that is not a single workflow"
}

// checkStatement reads the in-toto statement and finds name with digest in it.
func checkStatement(payloadType string, payload []byte, name string, digest [32]byte) error {
	if payloadType != inTotoPayloadType {
		return fmt.Errorf("%w: payload type %q", errUnsupportedFormat, payloadType)
	}
	var st struct {
		Type    string `json:"_type"`
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string `json:"predicateType"`
	}
	if err := json.Unmarshal(payload, &st); err != nil {
		return fmt.Errorf("the attested statement is not readable: %w", err)
	}
	if st.Type != statementType || st.PredicateType != slsaProvenanceV1 {
		return fmt.Errorf("%w: a %s statement about %s", errUnsupportedFormat, st.Type, st.PredicateType)
	}
	want := hex.EncodeToString(digest[:])
	for _, s := range st.Subject {
		if s.Name != name {
			continue
		}
		if strings.EqualFold(s.Digest["sha256"], want) {
			return nil
		}
		return fmt.Errorf("%w: the release's %s has a different SHA-256 (%s) than this one (%s)", errNotThisArtefact, name, s.Digest["sha256"], want)
	}
	return fmt.Errorf("%w: it lists no file called %s", errNotThisArtefact, name)
}

// pae is DSSE's pre-authentication encoding: what is actually signed.
func pae(payloadType string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	buf.Write(payload)
	return buf.Bytes()
}

// hashFor is the digest an ECDSA key of this size signs: SHA-256 for P-256, SHA-384 for P-384.
func hashFor(pub *ecdsa.PublicKey, msg []byte) []byte {
	return digestFor(pub.Curve.Params().BitSize, msg)
}

// parseECDSAKey reads one of the embedded PKIX keys.
func parseECDSAKey(b64 string) (*ecdsa.PublicKey, []byte, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, nil, errors.New("not an ECDSA key")
	}
	id := sha256.Sum256(der)
	return pub, id[:], nil
}

// pemCertDER returns the DER inside a PEM certificate, which is how Rekor's entry body quotes it.
func pemCertDER(p []byte) []byte {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	return block.Bytes
}

// jsonInt64 reads an int64 that protobuf JSON writes as a string.
type jsonInt64 int64

func (n *jsonInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = jsonInt64(v)
	return nil
}
