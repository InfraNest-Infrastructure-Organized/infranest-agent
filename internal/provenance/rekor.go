package provenance

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// verifyTlogEntry proves the signature is in Rekor, and returns the time Rekor says it arrived.
//
// Two independent proofs, both required, because they answer different questions. The signed entry
// timestamp is Rekor promising "I logged exactly this body at this time" — it is the only thing that
// authenticates the time, and the time is what the ten-minute certificate is judged at. The inclusion
// proof and its signed checkpoint show the entry really is in the append-only tree everybody can audit,
// rather than only promised to be. Then the entry's body is matched to this bundle, so a genuine entry for
// some other signature cannot be presented alongside a forged one.
func verifyTlogEntry(e tlogEntry, payload, sig, certDER []byte) (time.Time, error) {
	key, keyID, err := parseECDSAKey(rekorKey)
	if err != nil {
		return time.Time{}, err
	}
	if !bytes.Equal(e.LogID.KeyID, keyID) {
		return time.Time{}, fmt.Errorf("%w: it was logged in a transparency log this agent does not know", errUnsupportedFormat)
	}

	signedAt := time.Unix(int64(e.IntegratedTime), 0).UTC()
	if signedAt.Before(rekorValidFrom) {
		return time.Time{}, errors.New("the log entry predates the transparency log's key")
	}

	set, err := json.Marshal(struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
	}{
		Body:           base64.StdEncoding.EncodeToString(e.CanonicalizedBody),
		IntegratedTime: int64(e.IntegratedTime),
		LogID:          hex.EncodeToString(e.LogID.KeyID),
		LogIndex:       int64(e.LogIndex),
	})
	if err != nil {
		return time.Time{}, err
	}
	setHash := sha256.Sum256(set)
	if !ecdsa.VerifyASN1(key, setHash[:], e.InclusionPromise.SignedEntryTimestamp) {
		return time.Time{}, errors.New("the transparency log's signed timestamp does not verify")
	}

	if err := verifyInclusion(e, key, keyID); err != nil {
		return time.Time{}, err
	}

	if err := matchBody(e.CanonicalizedBody, payload, sig, certDER); err != nil {
		return time.Time{}, err
	}

	return signedAt, nil
}

// verifyInclusion checks the RFC 6962 audit path from the entry to the proof's root, and that Rekor signed
// a checkpoint for that root at that size.
func verifyInclusion(e tlogEntry, key *ecdsa.PublicKey, keyID []byte) error {
	p := e.InclusionProof
	index, size := uint64(p.LogIndex), uint64(p.TreeSize)
	if p.LogIndex < 0 || p.TreeSize <= 0 || index >= size {
		return errors.New("the inclusion proof's position is impossible")
	}

	leaf := sha256.Sum256(append([]byte{0}, e.CanonicalizedBody...))
	root, err := rootFromInclusionProof(index, size, leaf[:], p.Hashes)
	if err != nil {
		return err
	}
	if !bytes.Equal(root, p.RootHash) {
		return errors.New("the inclusion proof does not lead to the log's root")
	}

	cpSize, cpRoot, err := verifyCheckpoint(p.Checkpoint.Envelope, key, keyID)
	if err != nil {
		return err
	}
	if cpSize != size || !bytes.Equal(cpRoot, root) {
		return errors.New("the inclusion proof is for a different tree than the signed checkpoint")
	}
	return nil
}

func hashChildren(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

// rootFromInclusionProof is RFC 9162 §2.1.3.2, in the decomposed form transparency-dev/merkle uses: the
// proof's first `inner` hashes climb the subtree the leaf shares with the last leaf, the rest are the
// right border.
func rootFromInclusionProof(index, size uint64, leaf []byte, proof [][]byte) ([]byte, error) {
	inner := bits.Len64(index ^ (size - 1))
	border := bits.OnesCount64(index >> uint(inner))
	if len(proof) != inner+border {
		return nil, fmt.Errorf("the inclusion proof has %d hashes, not %d", len(proof), inner+border)
	}
	res := leaf
	for i, h := range proof[:inner] {
		if len(h) != sha256.Size {
			return nil, errors.New("the inclusion proof has a malformed hash")
		}
		if (index>>uint(i))&1 == 0 {
			res = hashChildren(res, h)
		} else {
			res = hashChildren(h, res)
		}
	}
	for _, h := range proof[inner:] {
		if len(h) != sha256.Size {
			return nil, errors.New("the inclusion proof has a malformed hash")
		}
		res = hashChildren(h, res)
	}
	return res, nil
}

// verifyCheckpoint checks a signed note (https://c2sp.org/signed-note) from rekor.sigstore.dev and returns
// the tree size and root hash it commits to.
func verifyCheckpoint(envelope string, key *ecdsa.PublicKey, keyID []byte) (uint64, []byte, error) {
	text, sigs, ok := strings.Cut(envelope, "\n\n")
	if !ok {
		return 0, nil, errors.New("the log checkpoint is malformed")
	}
	text += "\n"

	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], rekorOrigin+" - ") {
		return 0, nil, errors.New("the log checkpoint is not from rekor.sigstore.dev")
	}
	size, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil {
		return 0, nil, errors.New("the log checkpoint's size is malformed")
	}
	root, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil || len(root) != sha256.Size {
		return 0, nil, errors.New("the log checkpoint's root is malformed")
	}

	digest := sha256.Sum256([]byte(text))
	for _, line := range strings.Split(sigs, "\n") {
		rest, ok := strings.CutPrefix(line, "— "+rekorOrigin+" ")
		if !ok {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil || len(raw) < 5 || !bytes.Equal(raw[:4], keyID[:4]) {
			continue
		}
		if ecdsa.VerifyASN1(key, digest[:], raw[4:]) {
			return size, root, nil
		}
	}
	return 0, nil, errors.New("the log checkpoint is not signed by rekor.sigstore.dev")
}

// matchBody checks that the log entry is about this bundle: the same payload, the same signature, the
// same certificate. Without it, a genuine entry for somebody else's signature would pass every check above.
func matchBody(body, payload, sig, certDER []byte) error {
	var entry struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			PayloadHash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"payloadHash"`
			Signatures []struct {
				Signature []byte `json:"signature"`
				Verifier  []byte `json:"verifier"`
			} `json:"signatures"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &entry); err != nil {
		return fmt.Errorf("the log entry is not readable: %w", err)
	}
	if entry.Kind != "dsse" || entry.APIVersion != "0.0.1" {
		return fmt.Errorf("%w: the log entry is a %s/%s", errUnsupportedFormat, entry.Kind, entry.APIVersion)
	}
	ph := sha256.Sum256(payload)
	if entry.Spec.PayloadHash.Algorithm != "sha256" || entry.Spec.PayloadHash.Value != hex.EncodeToString(ph[:]) {
		return errors.New("the log entry is for a different statement")
	}
	if len(entry.Spec.Signatures) != 1 {
		return errors.New("the log entry does not record exactly one signature")
	}
	s := entry.Spec.Signatures[0]
	if !bytes.Equal(s.Signature, sig) {
		return errors.New("the log entry records a different signature")
	}
	if !bytes.Equal(pemCertDER(s.Verifier), certDER) {
		return errors.New("the log entry records a different certificate")
	}
	return nil
}

// digestFor hashes msg the way an ECDSA key of bitSize bits is used here.
func digestFor(bitSize int, msg []byte) []byte {
	if bitSize > 256 {
		h := sha512.Sum384(msg)
		return h[:]
	}
	h := sha256.Sum256(msg)
	return h[:]
}
