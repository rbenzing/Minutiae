package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// anchorKeys makes a key pair through the command line, in a directory outside any case.
func anchorKeys(t *testing.T) (priv, pub string) {
	t.Helper()
	priv = filepath.Join(t.TempDir(), "anchor.key")
	if code, out := run(t, Deps{}, "case", "anchor-keygen", "--out", priv); code != 0 || !strings.Contains(out, "public key fingerprint: sha256:") {
		t.Fatalf("anchor-keygen: %d %s", code, out)
	}
	return priv, priv + ".pub"
}

func auditBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var anchorHeadRe = regexp.MustCompile(`audit seq (\d+) sha256 ([0-9a-f]{64})`)

func TestCaseAnchorThenVerifyAnchorIsClean(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, byFile := range []bool{true, false} {
			dir := cleanVerifyCase(t)
			ext := t.TempDir()
			args := []string{"case", "anchor", "--case", dir}
			var priv, pub string
			if signed {
				priv, pub = anchorKeys(t)
				args = append(args, "--sign-key", priv)
			}
			file := filepath.Join(ext, "a.anchor")
			if byFile {
				args = append(args, "--out", file)
			}
			code, out := run(t, Deps{}, args...)
			m := anchorHeadRe.FindStringSubmatch(out)
			if code != 0 || m == nil || strings.Contains(out, "signed by") != signed {
				t.Fatalf("signed=%v file=%v: anchor exit %d: %s", signed, byFile, code, out)
			}
			vargs := []string{"case", "verify", "--case", dir}
			if byFile {
				vargs = append(vargs, "--anchor", file)
			} else {
				vargs = append(vargs, "--anchor-head", m[1]+":"+m[2])
			}
			if signed && byFile {
				vargs = append(vargs, "--anchor-key", pub)
			}
			code, out = run(t, Deps{}, vargs...)
			if code != 0 || !strings.Contains(out, "ANCHOR: matches up to audit seq "+m[1]) {
				t.Fatalf("signed=%v file=%v: verify exit %d: %s", signed, byFile, code, out)
			}
		}
	}
}

func TestCaseAnchorJSON(t *testing.T) {
	dir := cleanVerifyCase(t)
	code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--json")
	if code != 0 {
		t.Fatalf("anchor --json: %d %s", code, out)
	}
	if _, err := evidence.ParseAnchor([]byte(out)); err != nil {
		t.Fatalf("--json output is not an anchor file: %v\n%s", err, out)
	}
}

func TestCaseVerifyAnchorTamperExits4(t *testing.T) {
	dir := cleanVerifyCase(t)
	file := filepath.Join(t.TempDir(), "a.anchor")
	if code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--out", file); code != 0 {
		t.Fatalf("anchor: %d %s", code, out)
	}
	evidencetest.RechainAudit(t, dir, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		es[0].Actor = "forged-actor"
		return es
	})
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
		t.Fatalf("a rechained log is valid without an anchor: %d %s", code, out)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--anchor", file)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM: anchor: audit seq") || !strings.Contains(out, "ANCHOR: FAILED") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCaseVerifyAnchorOtherKeyExits4(t *testing.T) {
	dir := cleanVerifyCase(t)
	priv, _ := anchorKeys(t)
	_, otherPub := anchorKeys(t)
	file := filepath.Join(t.TempDir(), "a.anchor")
	if code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--sign-key", priv, "--out", file); code != 0 {
		t.Fatalf("anchor: %d %s", code, out)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--anchor", file, "--anchor-key", otherPub)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM: anchor:") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCaseAnchorOutInsideCaseIsUsage(t *testing.T) {
	dir := cleanVerifyCase(t)
	before := auditBytes(t, dir)
	for _, out := range []string{filepath.Join(dir, "a.anchor"), filepath.Join(dir, "sub", "..", "a.anchor"), filepath.Join(dir, "exports", "a.anchor")} {
		code, text := run(t, Deps{}, "case", "anchor", "--case", dir, "--out", out)
		if code != ExitUsage {
			t.Fatalf("--out %s: exit %d: %s", out, code, text)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("%s was written", out)
		}
	}
	if string(auditBytes(t, dir)) != string(before) {
		t.Fatal("the audit log changed")
	}
}

func TestCaseAnchorSignKeyInsideCaseIsUsage(t *testing.T) {
	dir := cleanVerifyCase(t)
	priv, _ := anchorKeys(t)
	key, err := os.ReadFile(priv)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(inside, key, 0o600); err != nil { //nolint:gosec // a test writes into its own temp case
		t.Fatal(err)
	}
	before := auditBytes(t, dir)
	code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--sign-key", inside)
	if code != ExitUsage || string(auditBytes(t, dir)) != string(before) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCaseAnchorOutExistingFileFails(t *testing.T) {
	dir := cleanVerifyCase(t)
	file := filepath.Join(t.TempDir(), "a.anchor")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--out", file)
	if b, _ := os.ReadFile(file); code != ExitError || string(b) != "keep" {
		t.Fatalf("exit %d, file %q: %s", code, b, out)
	}
}

func TestCaseAnchorKeygenRefusesExisting(t *testing.T) {
	d := t.TempDir()
	priv := filepath.Join(d, "k")
	if err := os.WriteFile(priv, []byte("old-priv"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "anchor-keygen", "--out", priv)
	if b, _ := os.ReadFile(priv); code != ExitError || string(b) != "old-priv" {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(priv + ".pub"); err == nil {
		t.Fatal("a public key was written next to a refused private key")
	}
	// the public file exists: nothing is written either
	priv2 := filepath.Join(d, "k2")
	if err := os.WriteFile(priv2+".pub", []byte("old-pub"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	code, out = run(t, Deps{}, "case", "anchor-keygen", "--out", priv2)
	if _, err := os.Stat(priv2); code != ExitError || err == nil {
		t.Fatalf("exit %d, private key written: %s", code, out)
	}
	if b, _ := os.ReadFile(priv2 + ".pub"); string(b) != "old-pub" {
		t.Fatalf("public key changed: %q", b)
	}
}

func TestCaseVerifyAnchorFlagsUsage(t *testing.T) {
	dir := cleanVerifyCase(t)
	_, pub := anchorKeys(t)
	before := auditBytes(t, dir)
	for _, args := range [][]string{
		{"--anchor", "x", "--anchor-head", "1:" + strings.Repeat("a", 64)},
		{"--anchor-key", pub},
	} {
		code, out := run(t, Deps{}, append([]string{"case", "verify", "--case", dir}, args...)...)
		if code != ExitUsage {
			t.Fatalf("%v: exit %d: %s", args, code, out)
		}
	}
	if string(auditBytes(t, dir)) != string(before) {
		t.Fatal("a usage error ran a verify")
	}
}

func TestCaseVerifyMalformedAnchorExits1BeforeVerifying(t *testing.T) {
	dir := cleanVerifyCase(t)
	bad := filepath.Join(t.TempDir(), "bad.anchor")
	if err := os.WriteFile(bad, []byte(`{"format":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--anchor", bad}, {"--anchor", filepath.Join(t.TempDir(), "missing")}, {"--anchor-head", "0:abc"}} {
		code, out := run(t, Deps{}, append([]string{"case", "verify", "--case", dir}, args...)...)
		if code != ExitError || strings.Contains(out, "OK:") {
			t.Fatalf("%v: exit %d: %s", args, code, out)
		}
	}
	if countAction(auditActions(t, dir), "verify.run") != 0 {
		t.Fatal("a verify ran with a malformed anchor")
	}
}

func TestCaseVerifyAnchorInsideCaseNotices(t *testing.T) {
	dir := cleanVerifyCase(t)
	file := filepath.Join(t.TempDir(), "a.anchor")
	if code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--out", file); code != 0 {
		t.Fatalf("anchor: %d %s", code, out)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "copy.anchor")
	if err := os.WriteFile(inside, b, 0o600); err != nil { //nolint:gosec // a test writes into its own temp case
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--anchor", inside)
	if !strings.Contains(out, "NOTICE:") || !strings.Contains(out, "inside the case") {
		t.Fatalf("exit %d, no inside-the-case notice: %s", code, out)
	}
}

// A missing audit log reaches the CLI as exit 4 (never a recreated log), for anchoring and for verifying.
func TestCaseAnchorMissingAuditLogExits4(t *testing.T) {
	dir := cleanVerifyCase(t)
	file := filepath.Join(t.TempDir(), "a.anchor")
	if code, out := run(t, Deps{}, "case", "anchor", "--case", dir, "--out", file); code != 0 {
		t.Fatalf("anchor: %d %s", code, out)
	}
	if err := os.Remove(filepath.Join(dir, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--anchor", file); code != ExitIntegrity {
		t.Fatalf("verify: exit %d: %s", code, out)
	}
	if code, out := run(t, Deps{}, "case", "anchor", "--case", dir); code != ExitIntegrity {
		t.Fatalf("anchor: exit %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Fatal("the audit log was recreated")
	}
}

func TestCaseAnchorOutputIsEscaped(t *testing.T) {
	dir := cleanVerifyCase(t)
	meta := filepath.Join(dir, "case.json")
	b, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"id":\s*"[^"]*"`)
	if !re.Match(b) {
		t.Fatalf("no id in case.json: %s", b)
	}
	if err := os.WriteFile(meta, re.ReplaceAll(b, []byte(`"id": "C\u001b[31mX"`)), 0o600); err != nil { //nolint:gosec // a test writes into its own temp case
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "case", "anchor", "--case", dir)
	if code != 0 || strings.ContainsRune(out, 0x1b) || !strings.Contains(out, `\x1b[31mX`) {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestInsideDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "case")
	if err := os.Mkdir(d, 0o750); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		d:                                true,
		filepath.Join(d, "a"):            true,
		filepath.Join(d, "x", "y", "z"):  true,
		filepath.Join(d, "x", "..", "a"): true,
		filepath.Join(d, ".."):           false,
		filepath.Join(d, "..", "other"):  false,
		d + "-sibling":                   false,
		filepath.Join(d+"-sibling", "a"): false,
	} {
		if got, err := insideDir(d, p); err != nil || got != want {
			t.Errorf("insideDir(%q) = %v, %v; want %v", p, got, err, want)
		}
	}
}
