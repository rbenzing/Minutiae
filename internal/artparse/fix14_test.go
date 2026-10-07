package artparse_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

func streamLimits() parse.Limits { return limitsWith(func(l *parse.Limits) { l.MemInputMax = 0 }) }

// I1: a streamed input is served only up to the size that was verified.
func TestStreamedInputNeverReadsPastTheVerifiedSize(t *testing.T) {
	f := newFixture(t, false, artparse.Options{Limits: streamLimits()})
	b := f.open(t)
	fh, err := os.OpenFile(f.file(f.db), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString("-EXTRA")
	_ = fh.Close()
	buf := make([]byte, 11)
	n, err := b.Input(false).Primary.R.ReadAt(buf, 0)
	if n != 5 || string(buf[:n]) != "hello" || !errors.Is(err, io.EOF) {
		t.Errorf("a grown file: read %d bytes %q, err %v; want the 5 verified bytes and EOF", n, buf[:n], err)
	}
	if n, err := b.Input(false).Primary.R.ReadAt(buf[:4], 5); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("a read at the verified end: n=%d err=%v, want 0 and EOF", n, err)
	}
	if err := b.Recheck(context.Background()); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("recheck of a grown file = %v, want the integrity error", err)
	}
}

// A change that is NOT undone is caught by the post-job re-hash, whatever the parser read meanwhile.
func TestStreamedChangeThatIsNotRestoredIsCaughtByRecheck(t *testing.T) {
	f := newFixture(t, false, artparse.Options{Limits: streamLimits()})
	b := f.open(t)
	writeFile(t, f.file(f.db), "HELLO")
	_ = readAll(t, b.Input(false).Primary.R, 5)
	var hm *artparse.HashMismatchError
	if err := b.Recheck(context.Background()); !errors.As(err, &hm) {
		t.Errorf("recheck = %v, want a hash mismatch", err)
	}
}

// The documented residual (CLAUDE.md section 9): a streamed input changed and restored before the
// re-hash is not detected; the exclusive case lock is the defence. job.end records streamed inputs.
func TestStreamedChangeThatIsRestoredIsTheDocumentedResidual(t *testing.T) {
	f := newFixture(t, false, artparse.Options{Limits: streamLimits()})
	b := f.open(t)
	writeFile(t, f.file(f.db), "HELLO")
	if got := readAll(t, b.Input(false).Primary.R, 5); got != "HELLO" {
		t.Fatalf("read %q", got)
	}
	writeFile(t, f.file(f.db), "hello")
	if err := b.Recheck(context.Background()); err != nil {
		t.Errorf("a restored file fails the recheck: %v (the residual is documented, not detected)", err)
	}
	if got := b.Streamed(); !slices.Equal(got, []string{f.db.ID}) {
		t.Errorf("Streamed = %v, want the primary %s", got, f.db.ID)
	}
}

func TestStreamedIsRecordedPerInput(t *testing.T) {
	f := newFixture(t, false, artparse.Options{Limits: parse.DefaultLimits()})
	if got := f.open(t).Streamed(); len(got) != 0 {
		t.Errorf("an in-memory bundle reports streamed inputs: %v", got)
	}
	f, others := lookupFixture(t, 4)
	_ = others
	b := f.open(t)
	in := b.Input(false)
	if _, err := in.Lookup.Open(parse.Artifact{ID: others[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := b.Lookups(); len(got) != 1 || got[0].Streamed {
		t.Errorf("an in-memory lookup open: %+v", got)
	}
	// MemInputMax 0: everything streams, lookup opens included
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "hello")
	o1 := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/o1.dat", "data-o1")
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, artparse.Options{Limits: streamLimits()})
	if err != nil {
		t.Fatal(err)
	}
	sf := &fixture{c: c, h: h, snap: snapshotOf(t, h), db: db}
	jobs, _, err := h.Discover(context.Background(), sf.snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	sf.job = jobs[0]
	sb := sf.open(t)
	if _, err := sb.Input(false).Lookup.Open(parse.Artifact{ID: o1.ID}); err != nil {
		t.Fatal(err)
	}
	if got := sb.Lookups(); len(got) != 1 || !got[0].Streamed {
		t.Errorf("a streamed lookup open: %+v", got)
	}
}

// I2: every Lookuper open counts against MemInputTotal.
func TestLookupOpensCountAgainstMemInputTotal(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "hello")
	o1 := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/o1.dat", "data-o1")
	o2 := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/o2.dat", "data-o2")
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.MemInputMax = 7; l.MemInputTotal = 14 })}
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{c: c, h: h, snap: snapshotOf(t, h), db: db}
	jobs, _, err := h.Discover(context.Background(), f.snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	f.job = jobs[0]
	in := f.open(t).Input(false) // 5 bytes in memory: 9 of the total left
	r1, err := in.Lookup.Open(parse.Artifact{ID: o1.ID})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := in.Lookup.Open(parse.Artifact{ID: o2.ID}) // 5+7+7 = 19 > 14: must stream
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.file(o1), "DATA-O1")
	writeFile(t, f.file(o2), "DATA-O2")
	if got := readAll(t, r1, 7); got != "data-o1" {
		t.Errorf("the first lookup open fits memory: read %q", got)
	}
	if got := readAll(t, r2, 7); got != "DATA-O2" {
		t.Errorf("the second would pass MemInputTotal and must stream: read %q", got)
	}
}

// I3: a lookup open hashes under the job's context and never under the bundle lock.
func TestLookupOpenStopsWhenTheJobContextEnds(t *testing.T) {
	f, others := lookupFixture(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opt artparse.Options
	opt.Limits = f.h.Limits()
	artparse.WithAfterSize(&opt, func(id string) {
		if id == others[0].ID {
			cancel() // the job ends while the lookup open is about to hash
		}
	})
	h, err := artparse.New(f.c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(ctx, snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	b, err := artparse.OpenBundle(ctx, h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	r, err := b.Input(false).Lookup.Open(parse.Artifact{ID: others[0].ID})
	if !errors.Is(err, context.Canceled) || r != nil {
		t.Errorf("Open under a cancelled job: reader %v, err %v, want context.Canceled", r, err)
	}
}

func TestCloseAndRecheckAreNotBlockedByALookupOpen(t *testing.T) {
	f, others := lookupFixture(t, 4)
	entered, release := make(chan struct{}), make(chan struct{})
	var opt artparse.Options
	opt.Limits = f.h.Limits()
	artparse.WithAfterSize(&opt, func(id string) {
		if id == others[0].ID {
			close(entered)
			<-release
		}
	})
	h, err := artparse.New(f.c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	opened := make(chan error, 1)
	go func() {
		_, err := b.Input(false).Lookup.Open(parse.Artifact{ID: others[0].ID})
		opened <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup open never started")
	}
	done := make(chan struct{})
	go func() {
		_ = b.Recheck(context.Background())
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("Recheck/Close are blocked behind a lookup open that is hashing")
	}
	close(release)
	if err := <-opened; !errors.Is(err, parse.ErrSealed) {
		t.Errorf("an open that finishes after Close = %v, want ErrSealed", err)
	}
}

// I4: ProbeBytes is one budget per Probe call, shared by the primary and every Lookuper open.
func TestProbeBudgetIsSharedWithLookupOpens(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	big := strings.Repeat("x", 10000)
	putFile(t, c, "D1", "A1", dbPath, big)
	o1 := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/o1.dat", big)
	opt := artparse.Options{Limits: limitsWith(func(l *parse.Limits) { l.ProbeBytes = 4096 })}
	h, err := artparse.New(c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	in := b.Input(true)
	buf := make([]byte, 3000)
	if n, err := in.Primary.R.ReadAt(buf, 0); n != 3000 || err != nil {
		t.Fatalf("primary read: %d %v", n, err)
	}
	r, err := in.Lookup.Open(parse.Artifact{ID: o1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadAt(buf[:2000], 0); n != 1096 || !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("a lookup open during Probe: n=%d err=%v, want the 1096 bytes left of the shared budget and ErrProbeLimit", n, err)
	}
	// the parse path is not limited
	r2, err := b.Input(false).Lookup.Open(parse.Artifact{ID: o1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r2.ReadAt(make([]byte, 10000), 0); n != 10000 || (err != nil && !errors.Is(err, io.EOF)) {
		t.Errorf("a parse-phase lookup open is limited: n=%d err=%v", n, err)
	}
}

func TestRecheckManifestCoversLookupOpens(t *testing.T) {
	f, others := lookupFixture(t, 4)
	b := f.open(t)
	if _, err := b.Input(false).Lookup.Open(parse.Artifact{ID: others[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := b.RecheckManifest(f.snap); err != nil {
		t.Fatalf("untouched: %v", err)
	}
	path := filepath.Join(f.c.Dir, "manifest.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	changed := false
	for i, l := range lines {
		if strings.Contains(l, others[1].ID) && strings.Contains(l, `"incomplete":false`) {
			lines[i] = strings.Replace(l, `"incomplete":false`, `"incomplete":true`, 1)
			changed = true
		}
	}
	if !changed {
		t.Fatal("the tamper did not apply")
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	if err := b.RecheckManifest(f.snap); !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("a lookup-opened artifact whose manifest record changed: %v", err)
	}
}

func TestCloseSealsEveryReader(t *testing.T) {
	f := newFixture(t, false, artparse.Options{Limits: parse.DefaultLimits()})
	b := f.open(t)
	in := b.Input(false)
	b.Close() // no explicit Seal
	if n, err := in.Primary.R.ReadAt(make([]byte, 2), 0); n != 0 || !errors.Is(err, parse.ErrSealed) {
		t.Errorf("a read after Close: n=%d err=%v, want ErrSealed", n, err)
	}
}

func TestLookupsReturnsACopy(t *testing.T) {
	f, others := lookupFixture(t, 4)
	b := f.open(t)
	if _, err := b.Input(false).Lookup.Open(parse.Artifact{ID: others[0].ID}); err != nil {
		t.Fatal(err)
	}
	got := b.Lookups()
	got[0].ArtifactID = "tampered"
	if again := b.Lookups(); again[0].ArtifactID != others[0].ID {
		t.Errorf("Lookups hands out the bundle's own slice: %v", again)
	}
}

func TestOpenBundleChecksTheContextBeforeEachMember(t *testing.T) {
	f := newFixture(t, true, artparse.Options{Limits: parse.DefaultLimits()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opt := artparse.Options{Limits: parse.DefaultLimits()}
	var opened []string
	artparse.WithOnOpen(&opt, func(id string) { opened = append(opened, id) })
	h, err := artparse.New(f.c, []artparse.Registered{register(t, "p", "1.0.0")}, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := artparse.OpenBundle(ctx, h, f.snap, f.job, "p1")
	if !errors.Is(err, context.Canceled) || b != nil {
		t.Errorf("bundle %v, err %v", b, err)
	}
	if len(opened) != 0 {
		t.Errorf("an artifact was opened under a cancelled context: %v", opened)
	}
}

func TestOpenBundleReleasesHandlesWhenAMemberFails(t *testing.T) {
	f := newFixture(t, true, artparse.Options{Limits: streamLimits()}) // the primary stays open (streamed)
	writeFile(t, f.file(f.wal), "WAL-X")                               // the second member fails its hash
	before := openFDs()
	if _, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1"); err == nil {
		t.Fatal("no error")
	}
	if before >= 0 {
		if after := openFDs(); after > before {
			t.Errorf("open file descriptors %d -> %d: a handle leaked", before, after)
		}
	}
	if err := os.Remove(f.file(f.db)); err != nil { // fails on Windows while a handle is open
		t.Errorf("the first member's handle was not closed: %v", err)
	}
}

// openFDs counts this process's descriptors on Linux, -1 elsewhere.
func openFDs() int {
	if runtime.GOOS != "linux" {
		return -1
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

func TestFileTruncatedAfterTheSizeCheckIsIntegrity(t *testing.T) {
	for name, keep := range map[string]int64{"to empty": 0, "to a prefix": 2} {
		t.Run(name, func(t *testing.T) {
			var path string
			var opt artparse.Options
			artparse.WithAfterSize(&opt, func(string) {
				if err := os.Truncate(path, keep); err != nil {
					t.Error(err)
				}
			})
			f := newFixture(t, false, opt)
			path = f.file(f.db)
			b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
			if !errors.Is(err, evidence.ErrIntegrity) || b != nil {
				t.Errorf("bundle %v, err %v", b, err)
			}
		})
	}
}

func TestMemorySourceShortReadReturnsEOF(t *testing.T) {
	for name, lim := range map[string]parse.Limits{"in memory": parse.DefaultLimits(), "streamed": streamLimits()} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, false, artparse.Options{Limits: lim})
			buf := make([]byte, 10)
			n, err := f.open(t).Input(false).Primary.R.ReadAt(buf, 0)
			if n != 5 || !errors.Is(err, io.EOF) {
				t.Errorf("a read past the end: n=%d err=%v, want 5 and io.EOF (the io.ReaderAt contract)", n, err)
			}
		})
	}
}

// errTail is a file that fails when read past the end.
type errTail struct {
	artparse.ReadFile
	err error
}

func (e errTail) Read(p []byte) (int, error) {
	n, err := e.ReadFile.Read(p)
	if n == 0 && errors.Is(err, io.EOF) {
		return 0, e.err
	}
	return n, err
}

func TestAnIOErrorOnTheGrowthProbeIsNotIntegrity(t *testing.T) {
	boom := errors.New("disk on fire")
	var opt artparse.Options
	artparse.WithWrapFile(&opt, func(f artparse.ReadFile) artparse.ReadFile { return errTail{f, boom} })
	f := newFixture(t, false, opt)
	b, err := artparse.OpenBundle(context.Background(), f.h, f.snap, f.job, "p1")
	if !errors.Is(err, boom) || errors.Is(err, evidence.ErrIntegrity) || b != nil {
		t.Errorf("bundle %v, err %v, want the plain I/O error", b, err)
	}
}

func TestLookupOpenOfAMemberReusesItsSource(t *testing.T) {
	var n atomic.Int32
	var opt artparse.Options
	artparse.WithOnOpen(&opt, func(string) { n.Add(1) })
	f := newFixture(t, false, opt)
	b := f.open(t)
	before := n.Load()
	if _, err := b.Input(false).Lookup.Open(parse.Artifact{ID: f.db.ID}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != before {
		t.Errorf("opening the bundle's own primary through the Lookuper loaded it again (%d opens)", n.Load()-before)
	}
}

func TestLookupFindReturnsTheManifestFacts(t *testing.T) {
	f, others := lookupFixture(t, 4)
	inc := putFile(t, f.c, "D1", "A1", "/data/data/com.a/databases/inc.dat", "partial")
	// mark the artifact incomplete in the manifest, the way a failed acquisition leaves it
	path := filepath.Join(f.c.Dir, "manifest.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i, l := range lines {
		if strings.Contains(l, inc.ID) {
			lines[i] = strings.Replace(l, `"incomplete":false`, `"incomplete":true`, 1)
		}
	}
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	h, err := artparse.New(f.c, []artparse.Registered{register(t, "p", "1.0.0")}, artparse.Options{Limits: parse.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	b, err := artparse.OpenBundle(context.Background(), h, snap, mustJob(t, h, snap), "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	found := b.Input(false).Lookup.Find(datGlob)
	if len(found) != len(others)+1 {
		t.Fatalf("found %v", ids(found))
	}
	recs, err := f.c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range found {
		i := slices.IndexFunc(recs, func(r evidence.ManifestRecord) bool { return r.ID == a.ID })
		if i < 0 {
			t.Fatalf("%s is not in the manifest", a.ID)
		}
		r := recs[i]
		if a.Size != r.Size || a.Incomplete != r.Incomplete || a.SHA256 != r.SHA256 || a.R != nil || a.Platform != parse.PlatformAndroid {
			t.Errorf("Find returned %+v for manifest record %+v", a, r)
		}
		if a.ID == inc.ID && !a.Incomplete {
			t.Error("an incomplete artifact is reported complete")
		}
	}
}

func mustJob(t *testing.T, h *artparse.Host, snap *artparse.Snapshot) artparse.Job {
	t.Helper()
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	return jobs[0]
}

func TestLookupFindNeverReturnsAnUnnamedArtifact(t *testing.T) {
	f, _ := lookupFixture(t, 4)
	unnamed := putFile(t, f.c, "D9", "A9", "/x/y", "no platform known for D9") // no acquire.start: no namer names it
	h, err := artparse.New(f.c, []artparse.Registered{register(t, "p", "1.0.0")}, artparse.Options{Limits: parse.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshotOf(t, h)
	b, err := artparse.OpenBundle(context.Background(), h, snap, mustJob(t, h, snap), "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, g := range []string{"**", "android:**", "ios:**", "*"} {
		if got := b.Input(false).Lookup.Find(g); slices.Contains(ids(got), unnamed.ID) {
			t.Errorf("glob %q returned the unnamed artifact", g)
		}
	}
}
