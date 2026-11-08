package provenance

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"time"
)

// verifySCT checks that the certificate carries a signed certificate timestamp from Sigstore's CT log —
// a promise, signed by the log, that it published this certificate (RFC 6962 §3.2). It is what makes a
// certificate issued in this workflow's name visible to anyone watching, whoever obtained it.
//
// The SCT signs the certificate as it was *before* the SCT was added, so that version is rebuilt here:
// the leaf's TBSCertificate with the SCT-list extension taken out.
func verifySCT(leaf, issuer *x509.Certificate) error {
	key, logID, err := parseECDSAKey(ctLogKey)
	if err != nil {
		return err
	}

	var list []byte
	for _, e := range leaf.Extensions {
		if e.Id.Equal(oidSCTList) {
			if _, err := asn1.Unmarshal(e.Value, &list); err != nil {
				return errors.New("the certificate's transparency timestamps are malformed")
			}
		}
	}
	if list == nil {
		return errors.New("the certificate carries no certificate-transparency timestamp")
	}

	tbs, err := tbsWithoutSCTs(leaf.RawTBSCertificate)
	if err != nil {
		return err
	}
	issuerKeyHash := sha256.Sum256(issuer.RawSubjectPublicKeyInfo)

	scts, ok := readU16Prefixed(list)
	if !ok || len(scts) == 0 {
		return errors.New("the certificate's transparency timestamps are malformed")
	}
	for len(scts) > 0 {
		var sct []byte
		sct, scts, ok = nextU16Prefixed(scts)
		if !ok {
			return errors.New("the certificate's transparency timestamps are malformed")
		}
		if verifyOneSCT(sct, key, logID, issuerKeyHash[:], tbs) {
			return nil
		}
	}
	return errors.New("the certificate was not logged in Sigstore's certificate-transparency log")
}

// verifyOneSCT checks one serialized SCT (RFC 6962 §3.2) against the one log we know.
func verifyOneSCT(sct []byte, key *ecdsa.PublicKey, logID, issuerKeyHash, tbs []byte) bool {
	// version(1) log_id(32) timestamp(8) extensions(2+n) hash(1) sig(1) signature(2+n)
	if len(sct) < 1+32+8+2 || sct[0] != 0 || !bytes.Equal(sct[1:33], logID) {
		return false
	}
	ts := binary.BigEndian.Uint64(sct[33:41])
	exts, rest, ok := nextU16Prefixed(sct[41:])
	if !ok || len(rest) < 2 || rest[0] != 4 || rest[1] != 3 { // sha256, ecdsa
		return false
	}
	sig, rest, ok := nextU16Prefixed(rest[2:])
	if !ok || len(rest) != 0 {
		return false
	}
	if time.UnixMilli(int64(ts)).Before(ctLogValidFrom) {
		return false
	}

	var signed bytes.Buffer
	signed.Write([]byte{0, 0}) // v1, certificate_timestamp
	binary.Write(&signed, binary.BigEndian, ts)
	signed.Write([]byte{0, 1}) // precert_entry
	signed.Write(issuerKeyHash)
	signed.Write([]byte{byte(len(tbs) >> 16), byte(len(tbs) >> 8), byte(len(tbs))})
	signed.Write(tbs)
	binary.Write(&signed, binary.BigEndian, uint16(len(exts)))
	signed.Write(exts)

	h := sha256.Sum256(signed.Bytes())
	return ecdsa.VerifyASN1(key, h[:], sig)
}

// tbsWithoutSCTs re-encodes a TBSCertificate with the SCT-list extension removed.
func tbsWithoutSCTs(raw []byte) ([]byte, error) {
	malformed := errors.New("the signing certificate is malformed")

	var tbs asn1.RawValue
	if rest, err := asn1.Unmarshal(raw, &tbs); err != nil || len(rest) != 0 {
		return nil, malformed
	}
	var fields []byte
	for rest := tbs.Bytes; len(rest) > 0; {
		var f asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &f); err != nil {
			return nil, malformed
		}
		if f.Class != asn1.ClassContextSpecific || f.Tag != 3 {
			fields = append(fields, f.FullBytes...)
			continue
		}

		var seq asn1.RawValue
		if r, err := asn1.Unmarshal(f.Bytes, &seq); err != nil || len(r) != 0 {
			return nil, malformed
		}
		var kept []byte
		for exts := seq.Bytes; len(exts) > 0; {
			var ext struct {
				ID       asn1.ObjectIdentifier
				Critical bool `asn1:"optional"`
				Value    []byte
			}
			var e asn1.RawValue
			if exts, err = asn1.Unmarshal(exts, &e); err != nil {
				return nil, malformed
			}
			if _, err := asn1.Unmarshal(e.FullBytes, &ext); err != nil {
				return nil, malformed
			}
			if !ext.ID.Equal(oidSCTList) {
				kept = append(kept, e.FullBytes...)
			}
		}
		inner, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: kept})
		if err != nil {
			return nil, malformed
		}
		wrapped, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 3, IsCompound: true, Bytes: inner})
		if err != nil {
			return nil, malformed
		}
		fields = append(fields, wrapped...)
	}
	return asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: fields})
}

// readU16Prefixed returns the body of a TLS vector with a two-byte length that must fill b.
func readU16Prefixed(b []byte) ([]byte, bool) {
	body, rest, ok := nextU16Prefixed(b)
	return body, ok && len(rest) == 0
}

func nextU16Prefixed(b []byte) (body, rest []byte, ok bool) {
	if len(b) < 2 {
		return nil, nil, false
	}
	n := int(binary.BigEndian.Uint16(b))
	if len(b) < 2+n {
		return nil, nil, false
	}
	return b[2 : 2+n], b[2+n:], true
}
