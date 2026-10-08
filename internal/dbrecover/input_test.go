package dbrecover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func diskPath(c *evidence.Case, r evidence.ManifestRecord) string {
	return filepath.Join(c.Dir, filepath.FromSlash(r.Path))
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func openSet(t *testing.T, c *evidence.Case, db, wal, journal string) (*sealedSet, error) {
	t.Helper()
	man, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return openInputs(context.Background(), c, man, db, wal, journal)
}

func TestOpenInputsRefusesChangedBytes(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	b := imageBytes()
	b[100] ^= 0xff
	if err := os.WriteFile(diskPath(c, a), b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openSet(t, c, a.ID, "", "")
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.Is(err, ErrInputChanged) {
		t.Fatalf("err %v, want integrity and input-changed", err)
	}
	if !strings.Contains(err.Error(), a.ID) {
		t.Errorf("message %q lacks the artifact id", err)
	}
}

func TestOpenInputsRefusesSizeMismatch(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	if err := os.WriteFile(diskPath(c, a), append(imageBytes(), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSet(t, c, a.ID, "", ""); !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("err %v, want an integrity error", err)
	}
}

func TestOpenInputsRefusesIneligibleAndUnknown(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	if _, err := openSet(t, c, "nope", "", ""); !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Errorf("unknown id: %v", err)
	}
	man, _ := c.Manifest()
	for i := range man {
		if man[i].ID == a.ID {
			man[i].Source.Kind = "report"
		}
	}
	_, err := openInputs(context.Background(), c, man, a.ID, "", "")
	var re *RefusalError
	if !errors.As(err, &re) || re.Reason != ReasonIneligibleKind {
		t.Fatalf("err %v, want an ineligible-kind refusal", err)
	}
}

func TestInputKindEligibility(t *testing.T) {
	ok := map[string]bool{"file": true, "backup": true, "extract": true, "recover": true, "carve": true}
	for _, k := range []string{"file", "backup", "extract", "recover", "carve", "runs", "report", "slack", "journal", "partition", "info", "serial", "unallocated", "import", ""} {
		got, why := eligibleKind(evidence.ManifestRecord{Source: evidence.Source{Kind: k}})
		if got != ok[k] {
			t.Errorf("kind %q eligible %v, want %v", k, got, ok[k])
		}
		if !got && why == "" {
			t.Errorf("kind %q refused without a reason", k)
		}
	}
}

// chain stores an image, a recovered artifact of confidence parentConf and a
// recovered artifact derived from it with childConf.
func chain(t *testing.T, c *evidence.Case, parentConf, childConf int, tag string) evidence.ManifestRecord {
	t.Helper()
	data := imageBytes()
	img := evidencetest.AddImage(t, c, data)
	p := evidencetest.AddRecovered(t, c, img, data, evidencetest.RecoveredSpec{
		Confidence: &parentConf, Runs: []evidence.Run{{Offset: 0, Length: 2048}}, Path: "recovered/p1-mtfs/" + tag + "-p.bin",
	})
	return evidencetest.AddRecovered(t, c, p, data[:2048], evidencetest.RecoveredSpec{
		Confidence: &childConf, Runs: []evidence.Run{{Offset: 0, Length: 1024}}, Path: "recovered/p1-mtfs/" + tag + "-c.bin",
	})
}

func TestInputCapIsChainMinimum(t *testing.T) {
	c := recordstest.NewCase(t)
	ch := chain(t, c, 40, 60, "000001")
	s, err := openSet(t, c, ch.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if got := s.cap(); got == nil || *got != 40 {
		t.Fatalf("cap %v, want the chain minimum 40", got)
	}
	live := recordstest.AddArtifact(t, c, "x.db", imageBytes())
	s2, err := openSet(t, c, live.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	if s2.cap() != nil || s2.mixed() || s2.incomplete() {
		t.Errorf("a live set: cap %v mixed %v incomplete %v", s2.cap(), s2.mixed(), s2.incomplete())
	}
}

func TestCapAppliesToWholeInputSet(t *testing.T) {
	c := recordstest.NewCase(t)
	live := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	img := evidencetest.AddImage(t, c, imageBytes())
	rec := func(conf int, path string) evidence.ManifestRecord {
		return evidencetest.AddRecovered(t, c, img, imageBytes(), evidencetest.RecoveredSpec{
			Confidence: &conf, Runs: []evidence.Run{{Offset: 0, Length: 2048}}, Path: path,
		})
	}
	wal := rec(45, "recovered/p1-mtfs/000001-app.db-wal")
	jr := rec(30, "recovered/p1-mtfs/000002-app.db-journal")
	s, err := openSet(t, c, live.ID, wal.ID, jr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if got := s.cap(); got == nil || *got != 30 {
		t.Fatalf("cap %v, want 30 over db, wal and journal", got)
	}
	if s.WAL == nil || s.Journal == nil || !s.WAL.Ref.Recovered || s.DB.Ref.Recovered {
		t.Errorf("refs %+v %+v %+v", s.DB.Ref, s.WAL, s.Journal)
	}
}

func TestMixedAndIncompleteFlags(t *testing.T) {
	c := recordstest.NewCase(t)
	live := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	rec := recoveredDB(t, c, 50, evidencetest.RecoveredSpec{Path: "recovered/p1-mtfs/000001-w"})
	inc := recordstest.AddIncompleteArtifact(t, c, "part.db", imageBytes())
	s, err := openSet(t, c, live.ID, rec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.mixed() || s.incomplete() {
		t.Errorf("live+recovered: mixed %v incomplete %v", s.mixed(), s.incomplete())
	}
	s.close()
	s, err = openSet(t, c, inc.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.mixed() || !s.incomplete() {
		t.Errorf("incomplete db: mixed %v incomplete %v", s.mixed(), s.incomplete())
	}
}

func TestInputsAreReadOnly(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	s, err := openSet(t, c, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.f.Write([]byte("x")); err == nil {
		t.Error("the held descriptor accepted a write")
	}
	s.close()
	got, err := os.ReadFile(diskPath(c, a))
	if err != nil || !bytes.Equal(got, imageBytes()) {
		t.Errorf("artifact bytes changed (%v)", err)
	}
}

func TestCheckUnchangedSeesChangedBytes(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	s, err := openSet(t, c, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.checkUnchanged(context.Background(), c); err != nil {
		t.Fatalf("unchanged input: %v", err)
	}
	b := imageBytes()
	b[7] ^= 1
	if err := os.WriteFile(diskPath(c, a), b, 0o600); err != nil {
		t.Fatal(err)
	}
	err = s.checkUnchanged(context.Background(), c)
	var ic *InputChangedError
	if !errors.As(err, &ic) || ic.ArtifactID != a.ID || ic.Want != a.SHA256 || ic.Got == "" || ic.Got == ic.Want {
		t.Fatalf("err %v", err)
	}
	if !errors.Is(err, ErrInputChanged) || !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("%v reaches neither ErrInputChanged nor ErrIntegrity", err)
	}
}

// Windows may refuse to remove an open file; the comparison itself is
// covered on every OS by the TestFreshCheck tests below (ruling J44).
func TestCheckUnchangedSeesReplacedFile(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	s, err := openSet(t, c, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	p := diskPath(c, a)
	if err := os.Remove(p); err != nil {
		t.Skipf("this OS does not remove an open file: %v", err)
	}
	b := imageBytes()
	b[9] ^= 1
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	err = s.checkUnchanged(context.Background(), c)
	var ic *InputChangedError
	if !errors.As(err, &ic) {
		t.Fatalf("err %v, want the replaced file seen by the re-open", err)
	}
}

func TestCheckUnchangedIOErrorIsNotIntegrity(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	s, err := openSet(t, c, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.DB.f.Close() // the held descriptor can no longer be read
	err = s.checkUnchanged(context.Background(), c)
	if err == nil {
		t.Fatal("a failed read passed")
	}
	if errors.Is(err, evidence.ErrIntegrity) || errors.Is(err, ErrInputChanged) {
		t.Errorf("an I/O error became an integrity verdict: %v", err)
	}
}

type failReader struct{ err error }

func (f failReader) Read([]byte) (int, error) { return 0, f.err }

func reopenOf(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

func TestFreshCheckComparesHeldAndReopened(t *testing.T) {
	old, repl := []byte("old bytes"), []byte("new bytes")
	want := sumHex(old)
	if err := freshCheck("A1", want, bytes.NewReader(old), reopenOf(old)); err != nil {
		t.Errorf("same bytes: %v", err)
	}
	// the held descriptor still reads the old bytes, the re-open sees a replacement
	err := freshCheck("A1", want, bytes.NewReader(old), reopenOf(repl))
	var ic *InputChangedError
	if !errors.As(err, &ic) || ic.Got != sumHex(repl) || ic.Want != want || ic.ArtifactID != "A1" {
		t.Errorf("replaced file: %v", err)
	}
	// the held bytes changed while a re-open still shows the original
	err = freshCheck("A1", want, bytes.NewReader(repl), reopenOf(old))
	if !errors.As(err, &ic) || ic.Got != sumHex(repl) {
		t.Errorf("changed held bytes: %v", err)
	}
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("mismatch is not an integrity error: %v", err)
	}
}

func TestFreshCheckIOErrorIsReturnedAsIs(t *testing.T) {
	boom := errors.New("disk fell over")
	old := []byte("old bytes")
	for name, err := range map[string]error{
		"held":    freshCheck("A1", sumHex(old), failReader{boom}, reopenOf(old)),
		"reopen":  freshCheck("A1", sumHex(old), bytes.NewReader(old), func() (io.ReadCloser, error) { return nil, boom }),
		"reading": freshCheck("A1", sumHex(old), bytes.NewReader(old), func() (io.ReadCloser, error) { return io.NopCloser(failReader{boom}), nil }),
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s: %v does not carry the I/O error", name, err)
		}
		if errors.Is(err, evidence.ErrIntegrity) || errors.Is(err, ErrInputChanged) {
			t.Errorf("%s: an I/O error became an integrity verdict", name)
		}
	}
}
