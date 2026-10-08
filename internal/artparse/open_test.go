package artparse_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// fixture is a case with the artifact at dbPath (and optionally its wal), a
// host and a snapshot, with one discovered job.
type fixture struct {
	c    *evidence.Case
	h    *artparse.Host
	snap *artparse.Snapshot
	job  artparse.Job
	db   evidence.ManifestRecord
	wal  evidence.ManifestRecord
}

func limitsWith(f func(*parse.Limits)) parse.Limits {
	l := parse.DefaultLimits()
	f(&l)
	return l
}

func newFixture(t *testing.T, withWal bool, opt artparse.Options) *fixture {
	t.Helper()
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	f := &fixture{c: c}
	f.db = putFile(t, c, "D1", "A1", dbPath, "hello")
	if withWal {
		f.wal = putFile(t, c, "D1", "A1", walPath, "wal-1")
	}
	if opt.Limits == (parse.Limits{}) {
		opt.Limits = parse.DefaultLimits()
	}
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	f.snap = snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), f.snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v, err %v", jobs, err)
	}
	f.job = jobs[0]
	return f
}

func (f *fixture) open(t *testing.T) *artparse.Bundle {
	t.Helper()
	b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "parse-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func (f *fixture) file(rec evidence.ManifestRecord) string {
	return filepath.Join(f.c.Dir, filepath.FromSlash(rec.Path))
}

func writeFile(t testing.TB, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil { //nolint:gosec // writes inside a temporary test case
		t.Fatal(err)
	}
}

func readAll(t testing.TB, r io.ReaderAt, size int64) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil reader")
	}
	buf := make([]byte, size)
	if n, err := r.ReadAt(buf, 0); err != nil && (!errors.Is(err, io.EOF) || int64(n) != size) {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestOpenBundleVerifiesHash(t *testing.T) {
	f := newFixture(t, true, artparse.Options{})
	in := f.open(t).Input(false)
	if got := readAll(t, in.Primary.R, 5); got != "hello" {
		t.Errorf("primary bytes %q", got)
	}
	if got := readAll(t, in.Artifacts["wal"].R, 5); got != "wal-1" {
		t.Errorf("wal bytes %q", got)
	}
	if in.Primary.ID != f.db.ID || in.Primary.SHA256 != f.db.SHA256 || in.Primary.Size != 5 ||
		in.Primary.Platform != parse.PlatformAndroid || in.Primary.Logical != "android:"+dbPath ||
		in.Artifacts["db"].ID != f.db.ID || in.Artifacts["wal"].ID != f.wal.ID {
		t.Errorf("input facts: %+v", in)
	}
	if in.Primary.Source.Kind != "file" || in.Primary.Source.DeviceID != "D1" || in.Primary.Source.RemotePath != dbPath {
		t.Errorf("source: %+v", in.Primary.Source)
	}
	if in.Job.ParseID != "parse-1" || in.Job.Parser.Name != "p" || in.Job.Job != f.job.N || in.Limits != f.h.Limits() || in.Budget == nil || in.Lookup == nil {
		t.Errorf("job, limits or handles: %+v", in)
	}
}

func TestOpenBundleRefusesHashMismatch(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	writeFile(t, f.file(f.db), "HELLO") // same size, other bytes
	b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
	var hm *artparse.HashMismatchError
	if !errors.As(err, &hm) || !errors.Is(err, evidence.ErrIntegrity) || b != nil {
		t.Fatalf("bundle %v, err %v, want a *HashMismatchError and no bundle", b, err)
	}
	if hm.ArtifactID != f.db.ID || hm.Want != f.db.SHA256 || hm.Got != sum("HELLO") || hm.Path != f.db.Path {
		t.Errorf("%+v", *hm)
	}
	for _, s := range []string{f.db.ID, f.db.SHA256, sum("HELLO")} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not name %q", err, s)
		}
	}
}

func TestOpenBundleSizeMismatchIsIntegrity(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	writeFile(t, f.file(f.db), "hello!")
	if _, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1"); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("%v", err)
	}
}

func TestOpenBundleMissingFileIsIntegrity(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	if err := os.Remove(f.file(f.db)); err != nil {
		t.Fatal(err)
	}
	if _, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1"); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("%v", err)
	}
}

func TestOpenBundleNotRegularFileIsIntegrity(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	if err := os.Remove(f.file(f.db)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.file(f.db), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1"); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("%v", err)
	}
}

func TestOpenBundleFileGrewIsIntegrity(t *testing.T) {
	var path string
	var opt artparse.Options
	artparse.WithAfterSize(&opt, func(string) { // after the size check, before the read
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fh.WriteString("more")
		_ = fh.Close()
	})
	for name, lim := range map[string]parse.Limits{
		"in memory": parse.DefaultLimits(),
		"streamed":  limitsWith(func(l *parse.Limits) { l.MemInputMax = 0 }),
	} {
		t.Run(name, func(t *testing.T) {
			opt.Limits = lim
			f := newFixture(t, false, opt)
			path = f.file(f.db)
			b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
			if !errors.Is(err, evidence.ErrIntegrity) || b != nil {
				t.Errorf("bundle %v, err %v", b, err)
			}
		})
	}
}

func TestOpenBundleOtherIOErrorIsNotIntegrity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file cannot be made unreadable with chmod on Windows; the classification itself is evidence.OpenArtifact's, tested there (TestOpenErrorClassification)")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	f := newFixture(t, false, artparse.Options{})
	if err := os.Chmod(f.file(f.db), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.file(f.db), 0o600) })
	_, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
	if err == nil || errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("err %v, want a plain I/O error", err)
	}
}

func TestSmallArtifactServedFromMemory(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	b := f.open(t)
	writeFile(t, f.file(f.db), "HELLO") // rewritten after the check
	in := b.Input(false)
	if got := readAll(t, in.Primary.R, 5); got != "hello" {
		t.Errorf("read %q after the file was rewritten, want the verified bytes", got)
	}
}

func TestLargeArtifactPreAndPostHashed(t *testing.T) {
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.MemInputMax = 0 })} // always stream
	f := newFixture(t, false, opt)
	b := f.open(t)
	if err := b.Recheck(context.Background()); err != nil {
		t.Errorf("an unchanged file failed the recheck: %v", err)
	}
	in := b.Input(false)
	if got := readAll(t, in.Primary.R, 5); got != "hello" {
		t.Errorf("read %q", got)
	}
	writeFile(t, f.file(f.db), "HELLO") // a change during the job
	if got := readAll(t, b.Input(false).Primary.R, 5); got != "HELLO" {
		t.Errorf("a streamed artifact is served from the handle: read %q, want the file's current bytes", got)
	}
	var hm *artparse.HashMismatchError
	if err := b.Recheck(context.Background()); !errors.As(err, &hm) || !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("recheck %v, want the integrity error", err)
	}
}

func TestMemInputTotalCapFallsBackToStreaming(t *testing.T) {
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.MemInputMax = 5; l.MemInputTotal = 5 })}
	f := newFixture(t, true, opt)
	b := f.open(t)
	writeFile(t, f.file(f.db), "HELLO")
	writeFile(t, f.file(f.wal), "WAL-1")
	in := b.Input(false)
	if got := readAll(t, in.Primary.R, 5); got != "hello" {
		t.Errorf("the first artifact fits memory and must be served from it: %q", got)
	}
	if got := readAll(t, in.Artifacts["wal"].R, 5); got != "WAL-1" {
		t.Errorf("the second would exceed MemInputTotal and must stream: %q", got)
	}
}

func TestRecheckAlsoCoversInMemoryInputs(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	b := f.open(t)
	if err := b.Recheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.file(f.db), "HELLO")
	if got := readAll(t, b.Input(false).Primary.R, 5); got != "hello" {
		t.Errorf("the parser must still see the verified bytes: %q", got)
	}
	if err := b.Recheck(context.Background()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("recheck %v, want the integrity error", err)
	}
}

func TestManifestSnapshotEqualityEverywhere(t *testing.T) {
	tamper := func(t *testing.T, f *fixture) {
		t.Helper()
		path := filepath.Join(f.c.Dir, "manifest.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out := strings.Replace(string(raw), `"incomplete":false`, `"incomplete":true`, 1)
		if out == string(raw) {
			t.Fatal("the manifest tamper did not apply")
		}
		writeFile(t, path, out)
	}
	t.Run("altered between the snapshot and openBundle", func(t *testing.T) {
		f := newFixture(t, false, artparse.Options{})
		tamper(t, f)
		b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
		if !errors.Is(err, evidence.ErrIntegrity) || b != nil || !strings.Contains(err.Error(), "manifest changed during the run") {
			t.Errorf("bundle %v, err %v", b, err)
		}
	})
	t.Run("altered between openBundle and recheckManifest", func(t *testing.T) {
		f := newFixture(t, false, artparse.Options{})
		b := f.open(t)
		tamper(t, f)
		if err := b.RecheckManifest(f.snap); !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "manifest changed during the run") {
			t.Errorf("%v", err)
		}
	})
	t.Run("untouched passes all three and the reads agree", func(t *testing.T) {
		f := newFixture(t, true, artparse.Options{})
		for i := 0; i < 3; i++ {
			recs, err := f.c.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) == 0 {
				t.Fatal("no records")
			}
			for _, r := range recs {
				if want, ok := f.snap.Record(r.ID); !ok || !f.snap.Matches(r) || want.SHA256 != r.SHA256 {
					t.Errorf("read %d: record %s differs from the snapshot", i, r.ID)
				}
			}
		}
		b := f.open(t)
		if err := b.RecheckManifest(f.snap); err != nil {
			t.Errorf("recheckManifest: %v", err)
		}
	})
}

func TestOpenBundleCallsOnOpenInOrder(t *testing.T) {
	var ids []string
	var opt artparse.Options
	artparse.WithOnOpen(&opt, func(id string) { ids = append(ids, id) })
	f := newFixture(t, true, opt)
	f.open(t)
	if len(ids) != 2 || ids[0] != f.db.ID || ids[1] != f.wal.ID {
		t.Errorf("onOpen saw %v, want the primary %s then the wal %s", ids, f.db.ID, f.wal.ID)
	}
}

func TestOpenBundleDoesNotModifyFiles(t *testing.T) {
	f := newFixture(t, true, artparse.Options{})
	snapshotFiles := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(f.c.Dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "case.lock") {
				return err
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p) //nolint:gosec // reads files of a temporary test case
			if err != nil {
				return err
			}
			out[p] = sum(string(data)) + st.ModTime().String()
			return nil
		})
		return out
	}
	before := snapshotFiles()
	b := f.open(t)
	_ = b.Input(true)
	_ = b.Input(false).Lookup.Find("android:/**")
	if err := b.Recheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.RecheckManifest(f.snap); err != nil {
		t.Fatal(err)
	}
	b.Close()
	after := snapshotFiles()
	if len(before) != len(after) {
		t.Fatalf("%d files before, %d after", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed", p)
		}
	}
}

func TestOpenBundleOpensOtherRolesInRoleOrder(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "hello")
	wal := putFile(t, c, "D1", "A1", walPath, "wal-1")
	jr := putFile(t, c, "D1", "A1", dbPath+"-journal", "jr")
	m := metaFor("three", "1.0.0")
	m.Inputs = append(m.Inputs, parse.InputSpec{Role: "journal", Companions: []string{"-journal"}})
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor("three"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	opt := artparse.Options{Limits: parse.DefaultLimits()}
	artparse.WithOnOpen(&opt, func(id string) { seen = append(seen, id) })
	h, err := artparse.New(c, []artparse.Registered{r}, opt)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 || len(jobs[0].Others) != 2 {
		t.Fatalf("jobs %+v err %v", jobs, err)
	}
	for i := 0; i < 20; i++ { // map iteration order is random: one pass could be right by luck
		seen = nil
		b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p")
		if err != nil {
			t.Fatal(err)
		}
		b.Close()
		if want := []string{db.ID, jr.ID, wal.ID}; !slices.Equal(seen, want) {
			t.Fatalf("opened %v, want the primary then the other roles by name (journal, wal): %v", seen, want)
		}
	}
}

func TestRecheckManifestRefusesADuplicatedRecord(t *testing.T) {
	f := newFixture(t, false, artparse.Options{})
	b := f.open(t)
	path := filepath.Join(f.c.Dir, "manifest.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw)+string(raw)) // every record twice: each still equals the snapshot's
	if err := b.RecheckManifest(f.snap); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("a duplicated record: %v, want the integrity error", err)
	}
}
