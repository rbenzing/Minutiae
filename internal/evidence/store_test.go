package evidence

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T, p string) *Store {
	t.Helper()
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreMigratesToV1(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	v, err := s.SchemaVersion()
	if err != nil || v != 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
	for _, table := range []string{"artifacts", "records"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
}

func TestStoreMigrationIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s1, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()
	s2 := openTestStore(t, p)
	var rows int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("schema_version rows = %d, %v", rows, err)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.db")
	s := openTestStore(t, p)
	if _, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := OpenStore(p); err == nil {
		t.Fatal("expected error for newer schema")
	}
}

func TestStoreInsertAndReadHashes(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "a.db"))
	r := ManifestRecord{
		ID: "id1", Path: "artifacts/d/a/x", Size: 3, SHA256: "s", MD5: "m",
		Source: Source{Kind: "file", DeviceID: "d", RemotePath: "/x"}, Finished: "2026-10-02T00:00:00Z",
	}
	if err := s.InsertArtifact(r); err != nil {
		t.Fatal(err)
	}
	h, err := s.ArtifactHashes()
	if err != nil || h["id1"] != "s" {
		t.Fatalf("hashes = %v, %v", h, err)
	}
}

func TestManifestAppendRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.jsonl")
	if rs, err := readManifest(p); err != nil || len(rs) != 0 {
		t.Fatalf("missing manifest should read empty: %v %v", rs, err)
	}
	for _, id := range []string{"a", "b"} {
		if err := appendManifest(p, ManifestRecord{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := readManifest(p)
	if err != nil || len(rs) != 2 || rs[1].ID != "b" {
		t.Fatalf("got %+v %v", rs, err)
	}
}
