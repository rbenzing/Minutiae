package evidence

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpenArtifactReadOnly(t *testing.T) {
	c := newTestCase(t)
	rec, err := c.Capture("dev1", "acq1", "a.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "0123456789")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f, got, err := c.OpenArtifact(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got.ID != rec.ID || got.SHA256 != rec.SHA256 {
		t.Fatalf("record = %+v, want %+v", got, rec)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatal("write to opened artifact succeeded")
	}
}

func TestOpenArtifactSizeMismatchIsIntegrityError(t *testing.T) {
	c, rec := caseWithArtifact(t)
	p := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if f, _, err := c.OpenArtifact(rec.ID); !errors.Is(err, ErrIntegrity) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("grown file: err = %v, want ErrIntegrity", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if f, _, err := c.OpenArtifact(rec.ID); !errors.Is(err, ErrIntegrity) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("deleted file: err = %v, want ErrIntegrity", err)
	}
}

func TestOpenArtifactUnknownID(t *testing.T) {
	c := newTestCase(t)
	f, _, err := c.OpenArtifact("nope")
	if !errors.Is(err, ErrUnknownArtifact) {
		t.Fatalf("err = %v, want ErrUnknownArtifact", err)
	}
	if f != nil {
		t.Fatal("file returned with error")
	}
}

func TestFindArtifactByIDOrPath(t *testing.T) {
	c, rec := caseWithArtifact(t)
	byID, err := c.FindArtifact(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	byPath, err := c.FindArtifact(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(byID, byPath) || byID.ID != rec.ID {
		t.Fatalf("byID = %+v, byPath = %+v", byID, byPath)
	}
	for _, ref := range []string{"unknown", "artifacts/dev1/acq1/missing.bin", ""} {
		if _, err := c.FindArtifact(ref); !errors.Is(err, ErrUnknownArtifact) {
			t.Errorf("FindArtifact(%q) err = %v, want ErrUnknownArtifact", ref, err)
		}
	}
}

func derivedSource(d *Derivation) Source {
	return Source{Kind: "extract", DeviceID: "dev1", Derived: d}
}

func TestDerivationRoundTripsThroughManifestAndAudit(t *testing.T) {
	c := newTestCase(t)
	want := &Derivation{
		ParentID: "p1", ParentSHA256: "abc", ParentIncomplete: true,
		Partition: 2, PartitionOffset: 1048576, FSType: "ext4", FSPath: "/etc/passwd", FSID: "uuid-1",
		Mode: 0o100644, UID: 1000, GID: 1000,
		Times:     map[string]string{"modified": "2024-01-02T03:04:05Z"},
		Encrypted: true,
		Runs:      []Run{{Offset: 4096, Length: 512}, {Offset: -1, Length: 1024}},
	}
	// A real parent, so the derivation verifies cleanly.
	parent, err := c.Capture("dev1", "acq0", "img.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "image")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want.ParentID, want.ParentSHA256 = parent.ID, parent.SHA256
	if _, err := c.Capture("dev1", "acq1", "f.bin", derivedSource(want), func(w io.Writer) error {
		_, err := io.WriteString(w, "data")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || !reflect.DeepEqual(recs[1].Source.Derived, want) {
		t.Fatalf("derived = %+v, want %+v", recs[len(recs)-1].Source.Derived, want)
	}
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("verify problems: %v", r.Problems)
	}
}

func TestVerifyFlagsDerivedArtifactWithBadParent(t *testing.T) {
	setup := func(t *testing.T) (*Case, ManifestRecord) {
		c := newTestCase(t)
		parent, err := c.Capture("dev1", "acq0", "img.bin", testSrc, func(w io.Writer) error {
			_, err := io.WriteString(w, "image")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return c, parent
	}
	capture := func(t *testing.T, c *Case, d *Derivation) {
		t.Helper()
		if _, err := c.Capture("dev1", "acq1", "f.bin", derivedSource(d), func(w io.Writer) error {
			_, err := io.WriteString(w, "data")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("good", func(t *testing.T) {
		c, p := setup(t)
		capture(t, c, &Derivation{ParentID: p.ID, ParentSHA256: p.SHA256})
		if r := mustVerify(t, c); !r.OK() {
			t.Fatalf("problems: %v", r.Problems)
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		c, p := setup(t)
		capture(t, c, &Derivation{ParentID: "ghost", ParentSHA256: p.SHA256})
		if r := mustVerify(t, c); r.OK() || !containsSubstr(r.Problems, "parent") {
			t.Fatalf("report = %+v", r)
		}
	})
	t.Run("parent hash differs", func(t *testing.T) {
		c, p := setup(t)
		capture(t, c, &Derivation{ParentID: p.ID, ParentSHA256: "00ff"})
		if r := mustVerify(t, c); r.OK() || !containsSubstr(r.Problems, "parent") {
			t.Fatalf("report = %+v", r)
		}
	})
	t.Run("missing runs artifact", func(t *testing.T) {
		c, p := setup(t)
		capture(t, c, &Derivation{ParentID: p.ID, ParentSHA256: p.SHA256, RunsArtifact: "ghost-runs"})
		if r := mustVerify(t, c); r.OK() || !containsSubstr(r.Problems, "parent") {
			t.Fatalf("report = %+v", r)
		}
	})
}
