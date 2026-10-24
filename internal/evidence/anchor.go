package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// AnchorFormat names the anchor file format and prefixes the signed message.
const AnchorFormat = "minutiae-audit-anchor/1"

// MaxAnchorFileBytes bounds the anchor and key files this package reads.
const MaxAnchorFileBytes = 64 << 10

const (
	maxAnchorText = 256 // bytes of the case id and the tool version
	anchorSigAlg  = "ed25519"
)

// ErrBadAnchor wraps every rejection of a malformed anchor (or head literal).
var ErrBadAnchor = errors.New("malformed audit anchor")

// AuditAnchor names one audit entry: its sequence number and hash, and the case it belongs to. It
// carries no verdict of any kind (never an "ok" or a verified stamp): verifying against it always
// runs every check.
type AuditAnchor struct {
	Format      string           `json:"format"`
	CaseID      string           `json:"case_id"` // "" only for a head literal
	Seq         int64            `json:"seq"`
	Hash        string           `json:"hash"`    // 64 lowercase hex
	Created     string           `json:"created"` // the anchored entry's time (RFC 3339)
	ToolVersion string           `json:"tool_version"`
	Signature   *AnchorSignature `json:"signature,omitempty"`
}

// AnchorSignature is an Ed25519 signature over the anchor payload (SignedMessage).
type AnchorSignature struct {
	Alg       string `json:"alg"`        // "ed25519"
	PublicKey string `json:"public_key"` // 64 lowercase hex
	Sig       string `json:"sig"`        // 128 lowercase hex
}

// anchorPayload is the signed part of an anchor: every field but the signature, in this order.
type anchorPayload struct {
	Format      string `json:"format"`
	CaseID      string `json:"case_id"`
	Seq         int64  `json:"seq"`
	Hash        string `json:"hash"`
	Created     string `json:"created"`
	ToolVersion string `json:"tool_version"`
}

func badAnchor(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadAnchor, fmt.Sprintf(format, a...))
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

var (
	anchorKeys    = []string{"format", "case_id", "seq", "hash", "created", "tool_version", "signature"}
	signatureKeys = []string{"alg", "public_key", "sig"}
)

// exactKeys requires b to start with a JSON object whose keys are all in allowed, spelled exactly
// (the standard decoder matches field names case-insensitively) and none repeated. For the key
// "signature" it also checks the nested object the same way.
func exactKeys(b []byte, allowed []string) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return badAnchor("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return badAnchor("%v", err)
		}
		key, _ := tok.(string)
		known := false
		for _, k := range allowed {
			known = known || k == key
		}
		if !known {
			return badAnchor("unknown or misspelled field %q", key)
		}
		if seen[key] {
			return badAnchor("field %q appears twice", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return badAnchor("%v", err)
		}
		if key == "signature" {
			if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
				return badAnchor("signature is not an object")
			}
			if err := exactKeys(raw, signatureKeys); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseAnchor decodes an anchor file: exactly one JSON object with exactly the known fields, no
// trailing data, at most MaxAnchorFileBytes, every field validated. Every error wraps ErrBadAnchor.
func ParseAnchor(b []byte) (AuditAnchor, error) {
	var a AuditAnchor
	if len(b) > MaxAnchorFileBytes {
		return a, badAnchor("larger than %d bytes", MaxAnchorFileBytes)
	}
	if err := exactKeys(b, anchorKeys); err != nil {
		return a, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return AuditAnchor{}, badAnchor("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return AuditAnchor{}, badAnchor("data after the anchor object")
	}
	if err := a.validate(); err != nil {
		return AuditAnchor{}, err
	}
	return a, nil
}

func (a AuditAnchor) validate() error {
	switch {
	case a.Format != AnchorFormat:
		return badAnchor("format %q, want %q", a.Format, AnchorFormat)
	case a.CaseID == "" || len(a.CaseID) > maxAnchorText:
		return badAnchor("case_id must be 1 to %d bytes", maxAnchorText)
	case a.Seq < 1:
		return badAnchor("seq %d is not positive", a.Seq)
	case !isLowerHex(a.Hash, 64):
		return badAnchor("hash is not 64 lowercase hex digits")
	case len(a.ToolVersion) > maxAnchorText:
		return badAnchor("tool_version is longer than %d bytes", maxAnchorText)
	}
	if _, err := time.Parse(time.RFC3339Nano, a.Created); err != nil || len(a.Created) > 64 {
		return badAnchor("created %q is not an RFC 3339 time", a.Created)
	}
	if s := a.Signature; s != nil {
		switch {
		case s.Alg != anchorSigAlg:
			return badAnchor("signature algorithm %q, want %q", s.Alg, anchorSigAlg)
		case !isLowerHex(s.PublicKey, 2*ed25519.PublicKeySize):
			return badAnchor("signature public_key is not 64 lowercase hex digits")
		case !isLowerHex(s.Sig, 2*ed25519.SignatureSize):
			return badAnchor("signature sig is not 128 lowercase hex digits")
		}
	}
	return nil
}

// ParseAnchorHead parses "SEQ:HASH", the paper form of an anchor: SEQ a canonical decimal in
// 1..MaxInt64, HASH 64 lowercase hex digits. The result names no case and carries no signature.
func ParseAnchorHead(s string) (AuditAnchor, error) {
	seqText, hash, ok := strings.Cut(s, ":")
	if !ok {
		return AuditAnchor{}, badAnchor("head %q is not SEQ:HASH", s)
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != seqText {
		return AuditAnchor{}, badAnchor("head sequence %q is not a canonical positive decimal", seqText)
	}
	if !isLowerHex(hash, 64) {
		return AuditAnchor{}, badAnchor("head hash is not 64 lowercase hex digits")
	}
	return AuditAnchor{Format: AnchorFormat, Seq: seq, Hash: hash}, nil
}

// Marshal is the file form: one JSON object and a newline.
func (a AuditAnchor) Marshal() []byte {
	b, err := json.Marshal(a)
	if err != nil { // cannot happen: strings, an integer and a pointer to strings
		panic(fmt.Sprintf("audit anchor: %v", err))
	}
	return append(b, '\n')
}

// SignedMessage is what the signature covers: the format line and the JSON of every field but the
// signature, recomputed from the decoded values.
func (a AuditAnchor) SignedMessage() []byte {
	b, err := json.Marshal(anchorPayload{a.Format, a.CaseID, a.Seq, a.Hash, a.Created, a.ToolVersion})
	if err != nil { // cannot happen
		panic(fmt.Sprintf("audit anchor: %v", err))
	}
	return append([]byte(AnchorFormat+"\n"), b...)
}

// SignAnchor returns a with an Ed25519 signature by key (replacing any signature it had).
func SignAnchor(a AuditAnchor, key ed25519.PrivateKey) AuditAnchor {
	a.Signature = nil
	sig := ed25519.Sign(key, a.SignedMessage())
	a.Signature = &AnchorSignature{
		Alg:       anchorSigAlg,
		PublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey)),
		Sig:       hex.EncodeToString(sig),
	}
	return a
}

// Signature check outcomes.
var (
	ErrAnchorUnsigned    = errors.New("anchor is not signed")
	ErrAnchorSignature   = errors.New("anchor signature does not verify")
	ErrAnchorKeyMismatch = errors.New("anchor is signed by another key")
)

// CheckSignature verifies the signature over the payload. With a trusted key the signature must be
// by exactly that key (ErrAnchorKeyMismatch otherwise); with trusted nil it is checked against the
// key the anchor carries, which proves only that the anchor was not edited since signing.
func (a AuditAnchor) CheckSignature(trusted ed25519.PublicKey) error {
	s := a.Signature
	if s == nil {
		return ErrAnchorUnsigned
	}
	pub, err1 := hex.DecodeString(s.PublicKey)
	sig, err2 := hex.DecodeString(s.Sig)
	if s.Alg != anchorSigAlg || err1 != nil || err2 != nil || len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return ErrAnchorSignature
	}
	if trusted != nil && !bytes.Equal(trusted, pub) {
		return ErrAnchorKeyMismatch
	}
	if !ed25519.Verify(pub, a.SignedMessage(), sig) {
		return ErrAnchorSignature
	}
	return nil
}

// KeyFingerprint is "sha256:" and the 64 lowercase hex digits of the SHA-256 of the raw public key.
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GenerateAnchorKey makes an Ed25519 key pair as PEM (private PKCS#8 "PRIVATE KEY", public PKIX
// "PUBLIC KEY") and the public key's fingerprint. The private key is NOT encrypted.
func GenerateAnchorKey(rand io.Reader) (privPEM, pubPEM []byte, fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate anchor key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}),
		KeyFingerprint(pub), nil
}

// pemBlock decodes b as exactly one PEM block of the given type, without headers.
func pemBlock(b []byte, typ string) ([]byte, error) {
	if len(b) > MaxAnchorFileBytes {
		return nil, fmt.Errorf("key file is larger than %d bytes", MaxAnchorFileBytes)
	}
	blk, rest := pem.Decode(b)
	switch {
	case blk == nil:
		return nil, errors.New("no PEM block")
	case blk.Type != typ:
		return nil, fmt.Errorf("PEM block is %q, want %q", blk.Type, typ)
	case len(blk.Headers) != 0:
		return nil, errors.New("PEM headers (an encrypted key) are not supported")
	case len(bytes.TrimSpace(rest)) != 0:
		return nil, errors.New("data after the PEM block")
	}
	return blk.Bytes, nil
}

// ParseAnchorPrivateKey reads a PKCS#8 "PRIVATE KEY" PEM holding an Ed25519 key.
func ParseAnchorPrivateKey(b []byte) (ed25519.PrivateKey, error) {
	der, err := pemBlock(b, "PRIVATE KEY")
	if err != nil {
		return nil, fmt.Errorf("anchor private key: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("anchor private key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok || len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("anchor private key: not an Ed25519 key")
	}
	return priv, nil
}

// ParseAnchorPublicKey reads a PKIX "PUBLIC KEY" PEM holding an Ed25519 key.
func ParseAnchorPublicKey(b []byte) (ed25519.PublicKey, error) {
	der, err := pemBlock(b, "PUBLIC KEY")
	if err != nil {
		return nil, fmt.Errorf("anchor public key: %w", err)
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("anchor public key: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("anchor public key: not an Ed25519 key")
	}
	return pub, nil
}
