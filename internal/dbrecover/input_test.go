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
	if err := s.checkUnchanged(context.Background()); err != nil {
		t.Fatalf("unchanged input: %v", err)
	}
	b := imageBytes()
	b[7] ^= 1
	if err := os.WriteFile(diskPath(c, a), b, 0o600); err != nil {
		t.Fatal(err)
	}
	err = s.checkUnchanged(context.Background())
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
	err = s.checkUnchanged(context.Background())
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
	err = s.checkUnchanged(context.Background())
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
	if err := freshCheck(context.Background(), "A1", want, bytes.NewReader(old), reopenOf(old)); err != nil {
		t.Errorf("same bytes: %v", err)
	}
	// the held descriptor still reads the old bytes, the re-open sees a replacement
	err := freshCheck(context.Background(), "A1", want, bytes.NewReader(old), reopenOf(repl))
	var ic *InputChangedError
	if !errors.As(err, &ic) || ic.Got != sumHex(repl) || ic.Want != want || ic.ArtifactID != "A1" {
		t.Errorf("replaced file: %v", err)
	}
	// the held bytes changed while a re-open still shows the original
	err = freshCheck(context.Background(), "A1", want, bytes.NewReader(repl), reopenOf(old))
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
		"held":    freshCheck(context.Background(), "A1", sumHex(old), failReader{boom}, reopenOf(old)),
		"reopen":  freshCheck(context.Background(), "A1", sumHex(old), bytes.NewReader(old), func() (io.ReadCloser, error) { return nil, boom }),
		"reading": freshCheck(context.Background(), "A1", sumHex(old), bytes.NewReader(old), func() (io.ReadCloser, error) { return io.NopCloser(failReader{boom}), nil }),
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s: %v does not carry the I/O error", name, err)
		}
		if errors.Is(err, evidence.ErrIntegrity) || errors.Is(err, ErrInputChanged) {
			t.Errorf("%s: an I/O error became an integrity verdict", name)
		}
	}
}

func TestCheckUnchangedReopensThroughOpenArtifact(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	w := recordstest.AddArtifact(t, c, "app.db-wal", append(imageBytes(), 1))
	s, err := openSet(t, c, a.ID, w.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var asked []string
	var got []*os.File
	orig := s.openArtifact
	s.openArtifact = func(id string) (*os.File, evidence.ManifestRecord, error) {
		asked = append(asked, id)
		f, r, err := orig(id)
		got = append(got, f)
		return f, r, err
	}
	if err := s.checkUnchanged(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != a.ID || asked[1] != w.ID {
		t.Fatalf("re-opened %v, want the db and the wal", asked)
	}
	if got[0] == s.DB.f || got[1] == s.WAL.f {
		t.Error("the re-open returned the held descriptor, not a fresh open")
	}
}

func TestCheckUnchangedSeesAReplacementFromTheReopen(t *testing.T) {
	c := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	s, err := openSet(t, c, a.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	b := imageBytes()
	b[11] ^= 1
	alt := filepath.Join(t.TempDir(), "alt.db")
	if err := os.WriteFile(alt, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.openArtifact = func(string) (*os.File, evidence.ManifestRecord, error) {
		f, err := os.Open(alt)
		return f, a, err
	}
	err = s.checkUnchanged(context.Background())
	var ic *InputChangedError
	if !errors.As(err, &ic) || ic.ArtifactID != a.ID || ic.Got != sumHex(b) {
		t.Fatalf("err %v, want the replacement seen through the re-open", err)
	}
}

func flipFirstByte(t *testing.T, c *evidence.Case, r evidence.ManifestRecord) {
	t.Helper()
	b, err := os.ReadFile(diskPath(c, r))
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(diskPath(c, r), b, 0o600); err != nil { //nolint:gosec // a path inside a temporary test case
		t.Fatal(err)
	}
}

func TestCheckUnchangedSeesChangedCompanions(t *testing.T) {
	for _, role := range []string{"wal", "journal"} {
		t.Run(role, func(t *testing.T) {
			c := recordstest.NewCase(t)
			db := recordstest.AddArtifact(t, c, "app.db", imageBytes())
			comp := recordstest.AddArtifact(t, c, "app.db-"+role, append(imageBytes(), 7))
			wal, jr := comp.ID, ""
			if role == "journal" {
				wal, jr = "", comp.ID
			}
			s, err := openSet(t, c, db.ID, wal, jr)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if err := s.checkUnchanged(context.Background()); err != nil {
				t.Fatalf("unchanged: %v", err)
			}
			flipFirstByte(t, c, comp)
			err = s.checkUnchanged(context.Background())
			var ic *InputChangedError
			if !errors.As(err, &ic) || ic.ArtifactID != comp.ID {
				t.Fatalf("err %v, want an InputChangedError naming %s", err, comp.ID)
			}
		})
	}
}

func TestMixedAndIncompleteSeeCompanions(t *testing.T) {
	c := recordstest.NewCase(t)
	live := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	liveWAL := recordstest.AddArtifact(t, c, "app.db-wal", append(imageBytes(), 1))
	conf := 50
	recDB := recoveredDB(t, c, 50, evidencetest.RecoveredSpec{Path: "recovered/p1-mtfs/000002-d"})
	man, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var img evidence.ManifestRecord
	for _, r := range man {
		if r.ID == recDB.Source.Derived.ParentID {
			img = r
		}
	}
	recJ := evidencetest.AddRecovered(t, c, img, imageBytes(), evidencetest.RecoveredSpec{
		Confidence: &conf, Runs: []evidence.Run{{Offset: 0, Length: 2048}}, Path: "recovered/p1-mtfs/000001-j",
	})
	incW := recordstest.AddIncompleteArtifact(t, c, "inc.db-wal", append(imageBytes(), 2))
	incJ := recordstest.AddIncompleteArtifact(t, c, "inc.db-journal", append(imageBytes(), 3))

	check := func(name, db, wal, jr string, mixed, incomplete bool) {
		s, err := openSet(t, c, db, wal, jr)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer s.close()
		if s.mixed() != mixed || s.incomplete() != incomplete {
			t.Errorf("%s: mixed %v incomplete %v, want %v %v", name, s.mixed(), s.incomplete(), mixed, incomplete)
		}
	}
	check("live db, recovered journal", live.ID, "", recJ.ID, true, false)
	check("live db and wal, recovered journal", live.ID, liveWAL.ID, recJ.ID, true, false)
	check("live db, live wal, incomplete journal", live.ID, liveWAL.ID, incJ.ID, false, true)
	check("recovered db, live wal", recDB.ID, liveWAL.ID, "", true, false)
	check("live db, incomplete wal", live.ID, incW.ID, "", false, true)
	check("live db, incomplete journal", live.ID, "", incJ.ID, false, true)
}

type cancelReader struct {
	cancel context.CancelFunc
	n, max int
}

func (r *cancelReader) Read(p []byte) (int, error) {
	r.n++
	if r.n == 3 {
		r.cancel()
	}
	if r.n > r.max {
		return 0, io.EOF
	}
	return len(p), nil
}

func TestHashingPollsTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelReader{cancel: cancel, max: 1 << 20}
	_, err := hashReader(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if r.n > 100 {
		t.Errorf("hash kept reading %d times after the cancel", r.n)
	}
	ctx, cancel = context.WithCancel(context.Background())
	old := []byte("old bytes")
	err = freshCheck(ctx, "A1", sumHex(old), &cancelReader{cancel: cancel, max: 1 << 20}, reopenOf(old))
	if !errors.Is(err, context.Canceled) || errors.Is(err, evidence.ErrIntegrity) || errors.Is(err, ErrInputChanged) {
		t.Errorf("a cancelled hash gave %v, want ctx.Err() and no integrity verdict", err)
	}
}

func TestOpenInputsRefusesOneArtifactInTwoRoles(t *testing.T) {
	c := recordstest.NewCase(t)
	db := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	w := recordstest.AddArtifact(t, c, "app.db-wal", append(imageBytes(), 1))
	for name, ids := range map[string][3]string{
		"db as wal":      {db.ID, db.ID, ""},
		"db as journal":  {db.ID, "", db.ID},
		"wal as journal": {db.ID, w.ID, w.ID},
	} {
		_, err := openSet(t, c, ids[0], ids[1], ids[2])
		var re *RefusalError
		if !errors.As(err, &re) || re.Reason != ReasonBadOption {
			t.Errorf("%s: err %v, want a bad-option refusal", name, err)
		}
	}
}

func TestRefusalErrorTextIncludesTheCause(t *testing.T) {
	boom := errors.New("the underlying cause")
	for _, e := range []*RefusalError{
		{Reason: ReasonBadOption, Err: boom},
		{Reason: ReasonBadOption, Detail: "a detail", Err: boom},
	} {
		if !strings.Contains(e.Error(), "the underlying cause") || !strings.HasPrefix(e.Error(), ReasonBadOption) {
			t.Errorf("%q lacks the cause or the reason", e.Error())
		}
	}
	if got := (&RefusalError{Reason: ReasonBadOption, Detail: "d"}).Error(); got != "bad-option: d" {
		t.Errorf("got %q", got)
	}
}
