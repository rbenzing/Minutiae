package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testAnchor() AuditAnchor {
	return AuditAnchor{
		Format: AnchorFormat, CaseID: "CASE-1", Seq: 7, Hash: strings.Repeat("ab", 32),
		Created: "2026-10-23T10:00:00.123456789Z", ToolVersion: "v1.2.3",
	}
}

func testKey(t testing.TB) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseAnchorRoundTrip(t *testing.T) {
	signed := SignAnchor(testAnchor(), testKey(t))
	for name, a := range map[string]AuditAnchor{"unsigned": testAnchor(), "signed": signed} {
		t.Run(name, func(t *testing.T) {
			b := a.Marshal()
			if !bytes.HasSuffix(b, []byte("}\n")) || bytes.Count(b, []byte("\n")) != 1 {
				t.Fatalf("file form = %q, want one JSON object and a newline", b)
			}
			got, err := ParseAnchor(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, a) {
				t.Errorf("round trip = %+v, want %+v", got, a)
			}
		})
	}
}

// anchorRejects lists the malformed anchors ParseAnchor must refuse (also the fuzz seeds).
func anchorRejects(t testing.TB) map[string][]byte {
	good := testAnchor()
	obj := func(mut func(m map[string]any)) []byte {
		b, err := json.Marshal(good)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		mut(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	sig := func(mut func(m map[string]any)) []byte {
		return obj(func(m map[string]any) {
			s := map[string]any{"alg": "ed25519", "public_key": strings.Repeat("0a", 32), "sig": strings.Repeat("1b", 64)}
			mut(s)
			m["signature"] = s
		})
	}
	return map[string][]byte{
		"unknown field":      obj(func(m map[string]any) { m["ok"] = true }),
		"trailing data":      append(good.Marshal(), []byte("x")...),
		"two objects":        append(good.Marshal(), good.Marshal()...),
		"too big":            append(good.Marshal(), bytes.Repeat([]byte(" "), MaxAnchorFileBytes)...),
		"wrong format":       obj(func(m map[string]any) { m["format"] = "minutiae-audit-anchor/2" }),
		"seq zero":           obj(func(m map[string]any) { m["seq"] = 0 }),
		"seq negative":       obj(func(m map[string]any) { m["seq"] = -1 }),
		"seq fraction":       obj(func(m map[string]any) { m["seq"] = 1.5 }),
		"hash short":         obj(func(m map[string]any) { m["hash"] = strings.Repeat("ab", 31) + "a" }),
		"hash upper":         obj(func(m map[string]any) { m["hash"] = strings.Repeat("AB", 32) }),
		"hash non hex":       obj(func(m map[string]any) { m["hash"] = strings.Repeat("zz", 32) }),
		"empty case id":      obj(func(m map[string]any) { m["case_id"] = "" }),
		"long case id":       obj(func(m map[string]any) { m["case_id"] = strings.Repeat("c", 257) }),
		"created not time":   obj(func(m map[string]any) { m["created"] = "yesterday" }),
		"created empty":      obj(func(m map[string]any) { m["created"] = "" }),
		"long tool version":  obj(func(m map[string]any) { m["tool_version"] = strings.Repeat("v", 257) }),
		"case variant key":   bytes.Replace(good.Marshal(), []byte(`"seq"`), []byte(`"Seq"`), 1),
		"duplicate key":      bytes.Replace(good.Marshal(), []byte(`"seq":7`), []byte(`"seq":7,"seq":8`), 1),
		"not an object":      []byte(`[1]`),
		"empty":              nil,
		"bad alg":            sig(func(s map[string]any) { s["alg"] = "rsa" }),
		"short key":          sig(func(s map[string]any) { s["public_key"] = strings.Repeat("0a", 31) }),
		"short sig":          sig(func(s map[string]any) { s["sig"] = strings.Repeat("1b", 63) }),
		"sig non hex":        sig(func(s map[string]any) { s["sig"] = strings.Repeat("zz", 64) }),
		"sig upper hex":      sig(func(s map[string]any) { s["sig"] = strings.Repeat("1B", 64) }),
		"sig unknown field":  sig(func(s map[string]any) { s["extra"] = 1 }),
		"signature not obj":  obj(func(m map[string]any) { m["signature"] = 5 }),
		"sig case variant":   bytes.Replace(SignAnchor(good, testKey(t)).Marshal(), []byte(`"alg"`), []byte(`"Alg"`), 1),
		"nul in unknown key": []byte(`{"format":"minutiae-audit-anchor/1","case_id":"a","seq":1,"hash":"` + strings.Repeat("a", 64) + `","created":"2026-10-23T10:00:00Z","tool_version":"","x\u0000":1}`),
	}
}

func TestParseAnchorRejects(t *testing.T) {
	for name, b := range anchorRejects(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAnchor(b); !errors.Is(err, ErrBadAnchor) {
				t.Fatalf("ParseAnchor(%q) error = %v, want ErrBadAnchor", b, err)
			}
		})
	}
}

func TestParseAnchorHead(t *testing.T) {
	h := strings.Repeat("ab", 32)
	a, err := ParseAnchorHead("12:" + h)
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != 12 || a.Hash != h || a.CaseID != "" || a.Signature != nil || a.Format != AnchorFormat {
		t.Errorf("head = %+v", a)
	}
	if _, err := ParseAnchorHead("9223372036854775807:" + h); err != nil {
		t.Errorf("MaxInt64 seq: %v", err)
	}
	for _, s := range []string{
		"012:" + h, "0:" + h, "-1:" + h, "+1:" + h, "12:" + strings.ToUpper(h), "12" + h, "12:" + h + ":x", "12:" + h[:63],
		"12: " + h, " 12:" + h, "9223372036854775808:" + h, ":" + h, "12:", "",
	} {
		if _, err := ParseAnchorHead(s); !errors.Is(err, ErrBadAnchor) {
			t.Errorf("ParseAnchorHead(%q) error = %v, want ErrBadAnchor", s, err)
		}
	}
}

func TestAnchorSignatureCovers(t *testing.T) {
	key := testKey(t)
	pub := key.Public().(ed25519.PublicKey)
	good := SignAnchor(testAnchor(), key)
	if err := good.CheckSignature(nil); err != nil {
		t.Fatalf("untouched: %v", err)
	}
	if err := good.CheckSignature(pub); err != nil {
		t.Fatalf("untouched with the signing key: %v", err)
	}
	other := testKey(t).Public().(ed25519.PublicKey)
	if err := good.CheckSignature(other); !errors.Is(err, ErrAnchorKeyMismatch) {
		t.Errorf("other trusted key: %v, want ErrAnchorKeyMismatch", err)
	}
	if err := testAnchor().CheckSignature(other); !errors.Is(err, ErrAnchorUnsigned) {
		t.Errorf("unsigned with a trusted key: %v, want ErrAnchorUnsigned", err)
	}
	if err := testAnchor().CheckSignature(nil); !errors.Is(err, ErrAnchorUnsigned) {
		t.Errorf("unsigned without a trusted key: %v, want ErrAnchorUnsigned", err)
	}
	muts := map[string]func(a *AuditAnchor){
		"format":       func(a *AuditAnchor) { a.Format = "minutiae-audit-anchor/2" },
		"case id":      func(a *AuditAnchor) { a.CaseID = "CASE-2" },
		"seq":          func(a *AuditAnchor) { a.Seq++ },
		"hash":         func(a *AuditAnchor) { a.Hash = strings.Repeat("cd", 32) },
		"created":      func(a *AuditAnchor) { a.Created = "2026-10-23T10:00:01Z" },
		"tool version": func(a *AuditAnchor) { a.ToolVersion = "v9" },
	}
	for name, mut := range muts {
		t.Run(name, func(t *testing.T) {
			a := good
			mut(&a)
			if err := a.CheckSignature(nil); !errors.Is(err, ErrAnchorSignature) {
				t.Errorf("CheckSignature(nil) = %v, want ErrAnchorSignature", err)
			}
			if err := a.CheckSignature(pub); !errors.Is(err, ErrAnchorSignature) {
				t.Errorf("CheckSignature(key) = %v, want ErrAnchorSignature", err)
			}
		})
	}
	// the signed message is the documented prefix and the payload without the signature
	if !bytes.HasPrefix(good.SignedMessage(), []byte("minutiae-audit-anchor/1\n{")) || bytes.Contains(good.SignedMessage(), []byte("signature")) {
		t.Errorf("signed message = %q", good.SignedMessage())
	}
	// a signature of another key over the same payload names that key, never the original
	re := SignAnchor(testAnchor(), testKey(t))
	if err := re.CheckSignature(pub); !errors.Is(err, ErrAnchorKeyMismatch) {
		t.Errorf("re-signed anchor against the first key: %v, want ErrAnchorKeyMismatch", err)
	}
}

func TestAnchorKeysPEM(t *testing.T) {
	privPEM, pubPEM, fp, err := GenerateAnchorKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParseAnchorPrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParseAnchorPublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != ed25519.PrivateKeySize || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("key sizes %d, %d", len(priv), len(pub))
	}
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), pub) {
		t.Error("the parsed private and public keys are not a pair")
	}
	sum := sha256.Sum256(pub)
	if want := "sha256:" + hex.EncodeToString(sum[:]); fp != want || KeyFingerprint(pub) != want {
		t.Errorf("fingerprint = %q / %q, want %q", fp, KeyFingerprint(pub), want)
	}
	if b, _ := pem.Decode(privPEM); b == nil || b.Type != "PRIVATE KEY" {
		t.Errorf("private PEM block = %v", b)
	}
	if b, _ := pem.Decode(pubPEM); b == nil || b.Type != "PUBLIC KEY" {
		t.Errorf("public PEM block = %v", b)
	}
	// an RSA PKCS#8 key and a PEM of another type are refused
	rk, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a test key only; it must be refused
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAnchorPrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err == nil {
		t.Error("an RSA private key was accepted")
	}
	pder, err := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAnchorPublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pder})); err == nil {
		t.Error("an RSA public key was accepted")
	}
	if _, err := ParseAnchorPrivateKey(pubPEM); err == nil {
		t.Error("a public key PEM was accepted as a private key")
	}
	if _, err := ParseAnchorPublicKey(privPEM); err == nil {
		t.Error("a private key PEM was accepted as a public key")
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err := ParseAnchorPublicKey(other); err == nil {
		t.Error("a certificate PEM was accepted")
	}
	big := append(append([]byte{}, privPEM...), bytes.Repeat([]byte("\n"), MaxAnchorFileBytes)...)
	if _, err := ParseAnchorPrivateKey(big); err == nil {
		t.Error("an oversize key file was accepted")
	}
	if _, err := ParseAnchorPrivateKey(nil); err == nil {
		t.Error("an empty key file was accepted")
	}
}

func TestAuditAppendLockedChecksTheHeadUnderTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := CreateAuditLog(path, "actor", "v0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	first, err := a.Append("case.create", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := a.appendLocked(func(int64, string) error { return boom }, "x.act", "", nil); !errors.Is(err, boom) {
		t.Fatalf("a failing check: %v, want it returned", err)
	}
	if n, problems, err := VerifyAuditLog(path); err != nil || n != 1 || len(problems) != 0 {
		t.Fatalf("after a refused append: n=%d problems=%v err=%v", n, problems, err)
	}
	var seenSeq int64
	var seenLast string
	e, err := a.appendLocked(func(seq int64, last string) error { seenSeq, seenLast = seq, last; return nil }, "x.act", "", map[string]any{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if seenSeq != first.Seq || seenLast != first.Hash || e.Seq != seenSeq+1 || e.PrevHash != seenLast {
		t.Errorf("check saw (%d, %s); the entry is seq %d prev %s", seenSeq, seenLast, e.Seq, e.PrevHash)
	}
	// Append is appendLocked without a check
	if _, err := a.Append("x.act2", "", nil); err != nil {
		t.Fatal(err)
	}
	if n, problems, err := VerifyAuditLog(path); err != nil || n != 3 || len(problems) != 0 {
		t.Fatalf("n=%d problems=%v err=%v", n, problems, err)
	}
	h, err := AuditEntryHash(e)
	if err != nil || h != e.Hash {
		t.Errorf("AuditEntryHash = %q, %v; want the entry's own hash %q", h, err, e.Hash)
	}
}

func TestBatchSetDigest(t *testing.T) {
	rows := []storedBatch{{ingest: "b", no: 1, digest: "d3"}, {ingest: "a", no: 2, digest: "d2"}, {ingest: "a", no: 10, digest: "d10"}, {ingest: "a", no: 1, digest: "d1"}}
	got := batchSetDigest(rows)
	var buf bytes.Buffer
	for _, r := range [][3]string{{"a", "1", "d1"}, {"a", "2", "d2"}, {"a", "10", "d10"}, {"b", "1", "d3"}} {
		buf.WriteString(r[0] + "\x00" + r[1] + "\x00" + r[2] + "\n")
	}
	sum := sha256.Sum256(buf.Bytes())
	if got.Count != 4 || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %+v, want 4 rows and %x", got, sum)
	}
	if rows[0].ingest != "b" {
		t.Error("batchSetDigest reordered the caller's rows")
	}
	empty := sha256.Sum256(nil)
	if e := batchSetDigest(nil); e.Count != 0 || e.SHA256 != hex.EncodeToString(empty[:]) {
		t.Errorf("empty set = %+v", e)
	}
}
