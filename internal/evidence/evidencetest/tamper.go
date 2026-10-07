package evidencetest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

const manifestName = "manifest.jsonl"

// RewriteManifest rewrites the one manifest.jsonl line of artifact id with
// mut applied to its record. Every other line stays byte-identical, and the
// audit log and artifacts.db are untouched, so only the manifest copy of the
// record disagrees with the rest of the case.
func RewriteManifest(t testing.TB, caseDir, id string, mut func(*evidence.ManifestRecord)) {
	t.Helper()
	p := filepath.Join(caseDir, manifestName)
	b, err := os.ReadFile(p) //nolint:gosec // test helper on a case directory the test created
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(b, []byte("\n"))
	found := false
	for i, l := range lines {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var r evidence.ManifestRecord
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		if r.ID != id {
			continue
		}
		found = true
		mut(&r)
		nl, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = nl
	}
	if !found {
		t.Fatalf("evidencetest: artifact %q is not in the manifest", id)
	}
	if err := os.WriteFile(p, bytes.Join(lines, []byte("\n")), 0o600); err != nil { //nolint:gosec // test helper on a case directory the test created
		t.Fatal(err)
	}
}

// FlipArtifactByte inverts the byte at offset off of the artifact's file,
// leaving the manifest, audit log and artifacts.db alone.
func FlipArtifactByte(t testing.TB, caseDir string, rec evidence.ManifestRecord, off int64) {
	t.Helper()
	p := filepath.Join(caseDir, filepath.FromSlash(rec.Path))
	f, err := os.OpenFile(p, os.O_RDWR, 0) //nolint:gosec // test helper on a case directory the test created
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}
