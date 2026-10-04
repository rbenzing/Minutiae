package evidence

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func caseWithArtifact(t *testing.T) (*Case, ManifestRecord) {
	t.Helper()
	c := newTestCase(t)
	rec, err := c.Capture("dev1", "acq1", "a.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "evidence")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func mustVerify(t *testing.T, c *Case) VerifyReport {
	t.Helper()
	r, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifyCleanCase(t *testing.T) {
	c, _ := caseWithArtifact(t)
	r := mustVerify(t, c)
	if !r.OK() || r.ArtifactsChecked != 1 || r.AuditEntries == 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsModifiedArtifact(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, filepath.FromSlash(rec.Path)), []byte("evidencE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); r.OK() || !containsSubstr(r.Problems, "hash mismatch") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsDeletedArtifact(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); r.OK() {
		t.Fatal("deleted artifact not detected")
	}
}

func TestVerifyDetectsUnmanifestedFile(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, artifactsDir, "planted.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !containsSubstr(r.Problems, "unmanifested") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsManifestLineRemoved(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := os.WriteFile(filepath.Join(c.Dir, manifestFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !containsSubstr(r.Problems, "not in manifest") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsAuditTamper(t *testing.T) {
	c, _ := caseWithArtifact(t)
	p := filepath.Join(c.Dir, auditFile)
	rewrite(t, p, func(l [][]byte) [][]byte { return l[1:] })
	if r := mustVerify(t, c); r.OK() {
		t.Fatal("audit tamper not detected")
	}
}

func TestVerifyAppendsVerifyRun(t *testing.T) {
	c, _ := caseWithArtifact(t)
	mustVerify(t, c)
	es, _ := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if es[len(es)-1].Action != "verify.run" {
		t.Fatalf("last action = %s", es[len(es)-1].Action)
	}
}

// rewriteManifest applies f to every manifest record and writes the file back.
func rewriteManifest(t *testing.T, c *Case, f func(r *ManifestRecord)) {
	t.Helper()
	p := filepath.Join(c.Dir, manifestFile)
	rewrite(t, p, func(lines [][]byte) [][]byte {
		for i, l := range lines {
			var r ManifestRecord
			if err := json.Unmarshal(l, &r); err != nil {
				t.Fatal(err)
			}
			f(&r)
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			lines[i] = b
		}
		return lines
	})
}

// dropImmutabilityTriggers drops every immut_* trigger, as an attacker with file
// access would before tampering with a table.
func dropImmutabilityTriggers(t *testing.T, c *Case) {
	t.Helper()
	rows, err := c.store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'immut\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close() // the single connection must be free before the DROPs
	for _, n := range names {
		if _, err := c.store.db.Exec(`DROP TRIGGER "` + n + `"`); err != nil {
			t.Fatal(err)
		}
	}
}

func execDB(t *testing.T, c *Case, query string, args ...any) {
	t.Helper()
	dropImmutabilityTriggers(t, c)
	if _, err := c.store.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsConsistentForgeryViaAudit(t *testing.T) {
	c, rec := caseWithArtifact(t)
	full := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	if err := os.WriteFile(full, []byte("forged evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := HashFile(full)
	if err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, c, func(r *ManifestRecord) { r.Size, r.SHA256, r.MD5 = d.Size, d.SHA256, d.MD5 })
	execDB(t, c, `UPDATE artifacts SET size = ?, sha256 = ?, md5 = ? WHERE id = ?`, d.Size, d.SHA256, d.MD5, rec.ID)
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "does not match its artifact.create audit entry") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsArtifactErasedFromManifestAndDB(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, manifestFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	execDB(t, c, `DELETE FROM artifacts`)
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "artifact "+rec.ID+": in audit log (artifact.create) but not in manifest") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsManifestRecordWithoutAudit(t *testing.T) {
	c, _ := caseWithArtifact(t)
	forged := ManifestRecord{ID: "forged", Path: "artifacts/dev1/acq1/forged.bin", Source: testSrc}
	if err := os.WriteFile(filepath.Join(c.Dir, filepath.FromSlash(forged.Path)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := HashFile(filepath.Join(c.Dir, filepath.FromSlash(forged.Path)))
	if err != nil {
		t.Fatal(err)
	}
	forged.Size, forged.SHA256, forged.MD5 = d.Size, d.SHA256, d.MD5
	if err := appendManifest(filepath.Join(c.Dir, manifestFile), forged); err != nil {
		t.Fatal(err)
	}
	if err := c.store.InsertArtifact(forged); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "artifact forged: in manifest but has no artifact.create audit entry") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsDuplicateManifestID(t *testing.T) {
	c, rec := caseWithArtifact(t)
	if err := appendManifest(filepath.Join(c.Dir, manifestFile), rec); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "artifact "+rec.ID+": duplicate manifest id") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyCompletesWhenArtifactsDirMissing(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := os.RemoveAll(filepath.Join(c.Dir, artifactsDir)); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "artifacts directory unreadable") {
		t.Fatalf("report = %+v", r)
	}
	assertLastAction(t, c, "verify.run")
}

func TestVerifyCompletesWhenDBUnreadable(t *testing.T) {
	c, _ := caseWithArtifact(t)
	execDB(t, c, `DROP TABLE artifacts`)
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "artifacts.db unreadable") {
		t.Fatalf("report = %+v", r)
	}
	assertLastAction(t, c, "verify.run")
}

func TestVerifyDetectsDBHashDiffers(t *testing.T) {
	c, rec := caseWithArtifact(t)
	execDB(t, c, `UPDATE artifacts SET sha256 = ? WHERE id = ?`, strings.Repeat("0", 64), rec.ID)
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, "artifact "+rec.ID+": artifacts.db sha256 differs from manifest") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsDBRowNotInManifest(t *testing.T) {
	c, _ := caseWithArtifact(t)
	if err := c.store.InsertArtifact(ManifestRecord{ID: "extra", Path: "artifacts/x", Source: testSrc}); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, "artifact extra: in artifacts.db but not in manifest") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDetectsManifestRecordNotInDB(t *testing.T) {
	c, rec := caseWithArtifact(t)
	execDB(t, c, `DELETE FROM artifacts WHERE id = ?`, rec.ID)
	r := mustVerify(t, c)
	if !containsSubstr(r.Problems, "artifact "+rec.ID+": in manifest but not in artifacts.db") {
		t.Fatalf("report = %+v", r)
	}
}

func assertLastAction(t *testing.T, c *Case, want string) {
	t.Helper()
	es, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := es[len(es)-1].Action; got != want {
		t.Fatalf("last audit action = %s, want %s", got, want)
	}
}

func TestVerifyFlagsLeftoverStaging(t *testing.T) {
	c, _ := caseWithArtifact(t)
	staging := filepath.Join(c.Dir, "staging")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("an empty staging directory is not a problem: %+v", r)
	}
	if err := os.MkdirAll(filepath.Join(staging, "ACQ1", "U1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "ACQ1", "U1", "Manifest.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !slices.Contains(r.Problems, "leftover staging directory ACQ1 (unpromoted acquisition data)") {
		t.Fatalf("report = %+v", r)
	}
}

// derivedCase returns a case holding a parent image and a derived artifact
// whose provenance carries a path and runs.
func derivedCase(t *testing.T) (*Case, ManifestRecord) {
	t.Helper()
	c := newTestCase(t)
	parent, err := c.Capture("dev1", "acq0", "img.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "image")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &Derivation{
		ParentID: parent.ID, ParentSHA256: parent.SHA256, FSType: "ext4", FSPath: "/etc/passwd",
		Runs:     []Run{{Offset: 4096, Length: 4}},
		Snapshot: &SnapshotRef{Name: "S", Xid: 5},
	}
	rec, err := c.Capture("dev1", "acq1", "f.bin", derivedSource(d), func(w io.Writer) error {
		_, err := io.WriteString(w, "data")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("clean derived case: %v", r.Problems)
	}
	return c, rec
}

func TestVerifyDetectsSourceDifferingFromAudit(t *testing.T) {
	tamper := map[string]func(r *ManifestRecord){
		"derived fs path": func(r *ManifestRecord) { r.Source.Derived.FSPath = "/etc/shadow" },
		"derived runs":    func(r *ManifestRecord) { r.Source.Derived.Runs = []Run{{Offset: 8192, Length: 4}} },
		"derivation gone": func(r *ManifestRecord) { r.Source.Derived = nil },
		"snapshot gone":   func(r *ManifestRecord) { r.Source.Derived.Snapshot = nil },
		"snapshot xid":    func(r *ManifestRecord) { r.Source.Derived.Snapshot.Xid = 6 },
		"snapshot name":   func(r *ManifestRecord) { r.Source.Derived.Snapshot.Name = "other" },
		"original path":   func(r *ManifestRecord) { r.Source.OriginalPath = "/evil/disk.img" },
	}
	for name, f := range tamper {
		t.Run(name, func(t *testing.T) {
			c, rec := derivedCase(t)
			rewriteManifest(t, c, func(r *ManifestRecord) {
				if r.ID == rec.ID {
					f(r)
				}
			})
			r := mustVerify(t, c)
			if r.OK() || !containsSubstr(r.Problems, "artifact "+rec.ID+" ("+rec.Path+"): source differs from audit") {
				t.Fatalf("report = %+v", r)
			}
		})
	}
}
