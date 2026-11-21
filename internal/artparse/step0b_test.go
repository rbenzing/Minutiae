package artparse_test

import (
	"context"
	"errors"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func tamper(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) {
	t.Helper()
	file := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(file, data, 0o600); err != nil { //nolint:gosec // inside a temporary test case
		t.Fatal(err)
	}
}

func TestClaimedTablesIntegrityIsAnErrorNeverEmpty(t *testing.T) {
	for name, damage := range map[string]func(*testing.T, *evidence.Case, evidence.ManifestRecord){
		"tampered": tamper,
		"deleted": func(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord) {
			if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, rec, _ := claimHost(t, parse.Applicable, "sms")
			damage(t, h.Case(), rec)
			got, err := h.ClaimedTables(context.Background(), rec.ID)
			if !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("claims %+v err %v, want an error wrapping evidence.ErrIntegrity", got, err)
			}
		})
	}
}

func TestClaimedTablesReturnsUnparsedClaimsMarked(t *testing.T) {
	for _, st := range []string{parse.UnsupportedSchema, parse.Encrypted} {
		h, rec, r := claimHost(t, st, "sms")
		got, err := h.ClaimedTables(context.Background(), rec.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("probe %s: claims %+v err %v, want the one claim", st, got, err)
		}
		id := r.Identity()
		if got[0].Table != "sms" || got[0].Parser != id.Name || !got[0].Unparsed || !strings.Contains(got[0].Reason, st) {
			t.Errorf("probe %s: claim %+v is not marked unparsed with its reason", st, got[0])
		}
	}
	// a job discovery already marked unparsed (encrypted content) is not dropped either
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: "ext4", FSPath: dbPath, Encrypted: true}
	rec := put(t, c, "D1", "A1", "enc.db", evidence.Source{Kind: "extract", RemotePath: dbPath, Derived: d}, "ciphertext")
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "cp")
	got, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), rec.ID)
	if err != nil || len(got) != 1 || !got[0].Unparsed || !strings.Contains(got[0].Reason, "encrypted") {
		t.Fatalf("encrypted extract: claims %+v err %v", got, err)
	}
	// the live (parseable) claim stays unmarked
	h, rec2, _ := claimHost(t, parse.Applicable, "sms")
	got, err = h.ClaimedTables(context.Background(), rec2.ID)
	if err != nil || len(got) != 1 || got[0].Unparsed || got[0].Reason != "" {
		t.Fatalf("applicable: %+v %v", got, err)
	}
}

func TestTableClaimsAreUniqueAcrossParsersCaseInsensitively(t *testing.T) {
	c := newCase(t)
	a := regFake(t, &claimMapper{claimParser{name: "alpha", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "alpha")
	b := regFake(t, &claimMapper{claimParser{name: "bravo", status: parse.Applicable, claims: []string{"SMS"}}, []string{"SMS"}}, "bravo")
	_, err := artparse.New(c, []artparse.Registered{a, b}, artparse.Options{Limits: parse.DefaultLimits()})
	if err == nil {
		t.Fatal("New accepted two parsers claiming the same table")
	}
	for _, want := range []string{"alpha", "bravo", "sms"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// distinct tables are fine
	d := regFake(t, &claimMapper{claimParser{name: "delta", status: parse.Applicable, claims: []string{"calls"}}, []string{"calls"}}, "delta")
	if _, err := artparse.New(c, []artparse.Registered{a, d}, artparse.Options{Limits: parse.DefaultLimits()}); err != nil {
		t.Fatalf("distinct claims refused: %v", err)
	}
	// one parser claiming a table twice (a Meta Register would have refused) is a duplicate too
	e := artparse.WithClaims(regFake(t, &claimMapper{claimParser{name: "echo", status: parse.Applicable}, []string{"sms"}}, "echo"), []parse.TableClaim{{Role: "db", Table: "sms"}, {Role: "wal", Table: "Sms"}})
	if _, err := artparse.New(c, []artparse.Registered{e}, artparse.Options{Limits: parse.DefaultLimits()}); err == nil || !strings.Contains(err.Error(), "echo") {
		t.Fatalf("one parser, one table twice: %v", err)
	}
}

func TestClaimedTablesRefusesDuplicateClaimsItReaches(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", dbPath, "db")
	a := regFake(t, &claimMapper{claimParser{name: "alpha", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "alpha")
	b := regFake(t, &claimMapper{claimParser{name: "bravo", status: parse.Applicable, claims: []string{"SMS"}}, []string{"SMS"}}, "bravo")
	h := hostWith(t, c, nil, a)
	artparse.SetRegistry(h, a, b) // a registry New would have refused
	got, err := h.ClaimedTables(context.Background(), rec.ID)
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "bravo") {
		t.Fatalf("claims %+v err %v, want a duplicate-claim error naming both parsers", got, err)
	}
}

func TestClaimedTablesFilterByRole(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	db := putFile(t, c, "D1", "A1", dbPath, "db")
	wal := putFile(t, c, "D1", "A1", walPath, "wal")
	// Register only accepts claims on the primary role; the host still filters by role, so a claim on another role is not attributed to the primary
	r := artparse.WithClaims(regFake(t, &claimMapper{claimParser{name: "rc", status: parse.Applicable}, []string{"sms", "wal_pages"}}, "rc"), []parse.TableClaim{{Role: "db", Table: "sms"}, {Role: "wal", Table: "wal_pages"}})
	h := hostWith(t, c, nil, r)
	got, err := h.ClaimedTables(context.Background(), db.ID)
	if err != nil || len(got) != 1 || got[0].Table != "sms" || got[0].Role != "db" {
		t.Fatalf("primary artifact: claims %+v err %v, want only the db claim", got, err)
	}
	got, err = h.ClaimedTables(context.Background(), wal.ID)
	if err != nil || len(got) != 1 || got[0].Table != "wal_pages" || got[0].Role != "wal" {
		t.Fatalf("companion artifact: claims %+v err %v, want only the wal claim", got, err)
	}
}

func TestClaimedTablesOfOneArtifactAreNotMultipliedByItsNeighbours(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	first := putFile(t, c, "D1", "A1", dbPath, "one")
	putFile(t, c, "D1", "A1", "/data/data/com.b/databases/app.db", "two")
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "cp")
	got, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), first.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("claims %+v err %v, want exactly one (the neighbour's job is not this artifact's)", got, err)
	}
}

func TestClaimedTablesCoverSnapshotArtifacts(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putExtract(t, c, "D1", "A1", "snap.db", "ext4", dbPath, &evidence.SnapshotRef{Name: "snap1", Xid: 7})
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "cp")
	got, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), rec.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("snapshot artifact: claims %+v err %v, want its claim", got, err)
	}
}

func TestPlanRowOfAnIncompleteArtifactIsMarked(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("whole"), "complete")
	src := evidence.Source{Kind: "file", DeviceID: "D1", RemotePath: fakePath("cut")}
	rec, _ := c.Capture("D1", "A1", strings.TrimPrefix(fakePath("cut"), "/"), src, func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return errors.New("pull interrupted")
	})
	if !rec.Incomplete {
		t.Fatal("the fixture artifact is not incomplete")
	}
	regs := []artparse.Registered{
		regFake(t, parsertest.ProbeOnly{Name: "whole", Version: "1.0.0", Status: parse.Applicable}, "whole"),
		regFake(t, parsertest.ProbeOnly{Name: "cut", Version: "1.0.0", Status: parse.Applicable}, "cut"),
	}
	p := planOf(t, hostWith(t, c, nil, regs...), artparse.Selection{})
	if r := rowOf(t, p, "cut"); !r.Incomplete || r.Status != artparse.StatusNew {
		t.Errorf("incomplete artifact: %+v, want a planned (new) row marked Incomplete", r)
	}
	if r := rowOf(t, p, "whole"); r.Incomplete {
		t.Errorf("complete artifact marked incomplete: %+v", r)
	}
}

func TestPlanReasonsNeverCarryTheHostCasePath(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", fakePath("gone"), "x")
	if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatal(err)
	}
	h := hostWith(t, c, nil, regFake(t, parsertest.ProbeOnly{Name: "gone", Version: "1.0.0", Status: parse.Applicable}, "gone"))
	r := rowOf(t, planOf(t, h, artparse.Selection{}), "gone")
	if !r.Integrity || r.Reason == "" {
		t.Fatalf("row %+v", r)
	}
	for _, dir := range []string{c.Dir, filepath.ToSlash(c.Dir), strings.ReplaceAll(c.Dir, `\`, `\\`)} {
		if strings.Contains(r.Reason, dir) {
			t.Errorf("the reason carries the host path %q: %q", dir, r.Reason)
		}
	}
	if !strings.Contains(r.Reason, rec.Path) {
		t.Errorf("the reason does not carry the case-relative path %q: %q", rec.Path, r.Reason)
	}
}

// closeCounter counts the handles the host opened and closed.
type closeCounter struct {
	artparse.ReadFile
	closes *atomic.Int32
}

func (f closeCounter) Close() error { f.closes.Add(1); return f.ReadFile.Close() }

func streamingHost(t *testing.T, c *evidence.Case, opens, closes *atomic.Int32, ps ...artparse.Registered) *artparse.Host {
	t.Helper()
	return hostWith(t, c, func(o *artparse.Options) {
		o.Limits.MemInputMax = 0 // every input streams: the handle stays open until the bundle closes
		artparse.WithWrapFile(o, func(f artparse.ReadFile) artparse.ReadFile { opens.Add(1); return closeCounter{f, closes} })
	}, ps...)
}

func TestPlanClosesEveryBundle(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("ok"), "x")
	var opens, closes atomic.Int32
	h := streamingHost(t, c, &opens, &closes, regFake(t, parsertest.ProbeOnly{Name: "ok", Version: "1.0.0", Status: parse.Applicable}, "ok"))
	if r := rowOf(t, planOf(t, h, artparse.Selection{}), "ok"); r.Status != artparse.StatusNew {
		t.Fatalf("row %+v", r)
	}
	if opens.Load() == 0 || opens.Load() != closes.Load() {
		t.Errorf("opened %d handles, closed %d: Plan leaked a bundle", opens.Load(), closes.Load())
	}
}

func TestClaimedTablesCloseTheirBundles(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", dbPath, "db")
	var opens, closes atomic.Int32
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "cp")
	h := streamingHost(t, c, &opens, &closes, r)
	if got, err := h.ClaimedTables(context.Background(), rec.ID); err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if opens.Load() == 0 || opens.Load() != closes.Load() {
		t.Errorf("opened %d handles, closed %d: ClaimedTables leaked a bundle", opens.Load(), closes.Load())
	}
}

// keepReader stores the reader Probe was given.
type keepReader struct {
	name string
	kept io.ReaderAt
}

func (p *keepReader) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *keepReader) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	p.kept = in.Primary.R
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p *keepReader) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// The readers Probe was given are sealed before the job goes on: a goroutine Probe left behind cannot
// read during Parse.
func TestPrepareJobSealsTheProbeReaders(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("keep"), "x")
	kr := &keepReader{name: "keep"}
	h := hostWith(t, c, nil, regFake(t, kr, "keep"))
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", jobs, err)
	}
	pr, err := artparse.PrepareJob(context.Background(), h, snap, jobs[0], "p1")
	if err != nil || pr.Bundle == nil {
		t.Fatalf("prepare: %+v %v", pr, err)
	}
	defer pr.Bundle.Close()
	var b [1]byte
	if _, err := kr.kept.ReadAt(b[:], 0); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("a Probe reader still reads after Probe returned: %v", err)
	}
}

// coopProbe's Probe returns when its context ends.
type coopProbe struct {
	name   string
	onCtx  func(context.Context)
	status string
}

func (p *coopProbe) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *coopProbe) Probe(ctx context.Context, _ *parse.Input) (parse.Applicability, error) {
	if p.onCtx != nil {
		p.onCtx(ctx)
	}
	if p.status != "" {
		return parse.Applicability{Status: p.status}, nil
	}
	<-ctx.Done()
	return parse.Applicability{}, ctx.Err()
}
func (p *coopProbe) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func TestPlanProbeOutcomeReasonsArePinned(t *testing.T) {
	setup := func(t *testing.T, name string) (*evidence.Case, string) {
		c := newCase(t)
		acquireStart(t, c, "D1", "A1", "logical")
		putFile(t, c, "D1", "A1", fakePath(name), "x")
		return c, name
	}
	t.Run("timed out, cooperative", func(t *testing.T) {
		c, n := setup(t, "coop")
		h := hostWith(t, c, fastLimits, regFake(t, &coopProbe{name: n}, n))
		if r := rowOf(t, planOf(t, h, artparse.Selection{}), n); r.Status != artparse.StatusUnparsed || r.Reason != "probe failed: timed out" {
			t.Errorf("row %+v", r)
		}
	})
	t.Run("did not stop", func(t *testing.T) {
		c, n := setup(t, "hang")
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		h := hostWith(t, c, fastLimits, regFake(t, &hangProbe{name: n, release: release}, n))
		if r := rowOf(t, planOf(t, h, artparse.Selection{}), n); r.Status != artparse.StatusUnparsed || r.Reason != "probe failed: did not stop after its time limit" {
			t.Errorf("row %+v", r)
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		c, n := setup(t, "odd")
		h := hostWith(t, c, nil, regFake(t, &coopProbe{name: n, status: "weird"}, n))
		if r := rowOf(t, planOf(t, h, artparse.Selection{}), n); r.Status != artparse.StatusUnparsed || r.Reason != `probe failed: unknown status "weird"` {
			t.Errorf("row %+v", r)
		}
	})
	t.Run("cancelled from inside Probe", func(t *testing.T) {
		c, n := setup(t, "stop")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var opens, closes atomic.Int32
		p := &coopProbe{name: n, onCtx: func(context.Context) { cancel() }}
		h := streamingHost(t, c, &opens, &closes, regFake(t, p, n))
		if _, err := h.Plan(ctx, artparse.Selection{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Plan = %v, want context.Canceled (a cancelled Probe is not a probe failure)", err)
		}
		if opens.Load() == 0 || opens.Load() != closes.Load() {
			t.Errorf("opened %d handles, closed %d", opens.Load(), closes.Load())
		}
	})
}

func TestPlanRowIsIncompleteWhenACompanionIs(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("w"), "whole database")
	src := evidence.Source{Kind: "file", DeviceID: "D1", RemotePath: fakePath("w") + "-wal"}
	rec, _ := c.Capture("D1", "A1", strings.TrimPrefix(fakePath("w")+"-wal", "/"), src, func(w io.Writer) error {
		_, _ = io.WriteString(w, "cut wal")
		return errors.New("pull interrupted")
	})
	if !rec.Incomplete {
		t.Fatal("the companion is not incomplete")
	}
	p := planOf(t, hostWith(t, c, nil, regFake(t, walParser{name: "w"}, "w")), artparse.Selection{})
	if r := rowOf(t, p, "w"); !r.Incomplete || r.Status != artparse.StatusNew {
		t.Errorf("row %+v, want a planned row marked Incomplete (its companion is)", r)
	}
}

// Sealing what Probe was given must not seal the bundle: the Parse that follows needs its own readers
// and Lookuper, and a Lookuper Probe kept stays sealed.
func TestPrepareJobLeavesTheBundleUsableForParse(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	putFile(t, c, "D1", "A1", fakePath("keep"), "x")
	putFile(t, c, "D1", "A1", fakePath("sib"), "y")
	kl := &keepLookup{name: "keep"}
	h := hostWith(t, c, nil, regFake(t, kl, "keep"))
	snap := snapshotOf(t, h)
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{Parsers: []artparse.ParserRef{{Name: "keep"}}})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", jobs, err)
	}
	pr, err := artparse.PrepareJob(context.Background(), h, snap, jobs[0], "p1")
	if err != nil || pr.Bundle == nil {
		t.Fatalf("prepare: %+v %v", pr, err)
	}
	defer pr.Bundle.Close()
	in := pr.Bundle.Input(false)
	var b [1]byte
	if _, err := in.Primary.R.ReadAt(b[:], 0); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("the reader of the Parse input is sealed after Probe: %v", err)
	}
	found := in.Lookup.Find("android:**/parsertest/sib.dat")
	if len(found) != 1 {
		t.Fatalf("found %v", found)
	}
	if _, err := in.Lookup.Open(found[0]); err != nil {
		t.Errorf("the Lookuper of the Parse input is sealed after Probe: %v", err)
	}
	if _, err := kl.lookup.Open(found[0]); !errors.Is(err, parse.ErrSealed) {
		t.Errorf("the Lookuper Probe kept still opens: %v", err)
	}
}

// keepLookup keeps the Lookuper Probe was given.
type keepLookup struct {
	name   string
	lookup parse.Lookuper
}

func (p *keepLookup) Meta() parse.Meta { return parsertest.FakeMeta(p.name, "1.0.0") }
func (p *keepLookup) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	p.lookup = in.Lookup
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (p *keepLookup) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// The host uses the contract rules internal/parse shares with the strict test harness, not private
// copies: the record rule in the emitter, the re-hash of every input after a job.
func TestHostUsesTheSharedContractChecks(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "parse" {
					used[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	for _, rule := range []string{"CheckRecord", "CheckInputsUnchanged"} {
		if !used[rule] {
			t.Errorf("artparse does not use parse.%s", rule)
		}
	}
}

// 4I-R23: claim uniqueness is per platform. One artifact has one path scheme, so two parsers of different platforms can
// never claim on the same artifact: the same table name on two platforms is accepted, the same platform still refused.
func TestClaimUniquenessIsPerPlatform(t *testing.T) {
	c := newCase(t)
	mk := func(name, table string, platforms ...string) artparse.Registered {
		return artparse.WithPlatforms(regFake(t, &claimMapper{claimParser{name: name, status: parse.Applicable, claims: []string{table}}, []string{table}}, name), platforms...)
	}
	opts := artparse.Options{Limits: parse.DefaultLimits()}
	if _, err := artparse.New(c, []artparse.Registered{mk("droid", "message", parse.PlatformAndroid), mk("apple", "message", parse.PlatformIOS)}, opts); err != nil {
		t.Fatalf("one table on two platforms refused: %v", err)
	}
	_, err := artparse.New(c, []artparse.Registered{mk("droid", "message", parse.PlatformAndroid), mk("droid2", "MESSAGE", parse.PlatformAndroid)}, opts)
	if err == nil {
		t.Fatal("the same platform, the same table (case-folded): accepted")
	}
	for _, want := range []string{"droid", "droid2", "message", parse.PlatformAndroid} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// a parser on both platforms counts on both
	if _, err := artparse.New(c, []artparse.Registered{mk("both", "message", parse.PlatformAndroid, parse.PlatformIOS), mk("apple", "message", parse.PlatformIOS)}, opts); err == nil {
		t.Fatal("a two-platform parser and an iOS parser on one table: accepted")
	}
}

// globClaimMapper is a claimMapper whose primary role matches the given globs.
type globClaimMapper struct {
	claimMapper
	globs []string
}

func (p *globClaimMapper) Meta() parse.Meta {
	m := p.claimMapper.Meta()
	m.Inputs[0].Globs = p.globs
	return m
}

func globClaimer(t *testing.T, name, table string, globs ...string) artparse.Registered {
	t.Helper()
	return regFake(t, &globClaimMapper{claimMapper{claimParser{name: name, status: parse.Applicable, claims: []string{table}}, []string{table}}, globs}, name)
}

// 4I4-T10 (amends 4I-R23): two parsers of one platform may claim one table when every pair of their primary globs ends in
// different file names (two literals that differ, or a pattern that cannot match the other's literal): no artifact can be
// matched by both.
func TestClaimUniquenessAllowsDisjointFiles(t *testing.T) {
	c := newCase(t)
	opts := artparse.Options{Limits: parse.DefaultLimits()}
	a := globClaimer(t, "wa", "messages", "android:**/com.whatsapp/databases/msgstore.db", "android:**/WhatsApp/Databases/msgstore*.db", "android:**/WhatsApp/Databases/msgstore*.db.crypt*")
	b := globClaimer(t, "tg", "messages", "android:**/org.telegram.messenger/files/cache4.db", "android:**/org.telegram.messenger/files/account*/cache4.db")
	gm := globClaimer(t, "gm", "messages", "android:**/com.google.android.apps.messaging/databases/bugle_db", "android:**/com.android.messaging/databases/bugle_db")
	gmp := globClaimer(t, "gmp", "parts", "android:**/com.google.android.apps.messaging/databases/bugle_db", "android:**/com.android.messaging/databases/bugle_db") // parts is claimed by no other parser (4I5 spec 12)
	if _, err := artparse.New(c, []artparse.Registered{a, b, gm, gmp}, opts); err != nil {
		t.Fatalf("disjoint literal file names refused: %v", err)
	}
}

// 4I4-T10: a pattern that matches the other final segment, two patterns, or equal literals (also across case) may meet
// in one artifact: refused.
func TestClaimUniquenessRefusesFilesThatMayMeet(t *testing.T) {
	c := newCase(t)
	opts := artparse.Options{Limits: parse.DefaultLimits()}
	for name, pair := range map[string][2]string{
		"wildcard":     {"android:**/x/*.db", "android:**/y/cache4.db"},
		"equal":        {"android:**/x/cache4.db", "android:**/y/cache4.db"},
		"equal-fold":   {"android:**/x/Cache4.db", "android:**/y/cache4.db"},
		"star-literal": {"android:**/x/cache*.db", "android:**/y/cache4.db"},
		"brace":        {"android:**/x/{a,b}.db", "android:**/y/c.db"},
		"double-star":  {"android:x/**", "android:y/cache4.db"},
		"two-patterns": {"android:**/x/msgstore*.db", "android:**/y/cache*.db"},
		"second-pair":  {"android:**/x/a.db", "android:**/x/b.db"},
	} {
		a := globClaimer(t, "pa", "messages", pair[0])
		b := globClaimer(t, "pb", "MESSAGES", pair[1])
		if name == "second-pair" { // one disjoint pair is not enough: every pair must be
			a = globClaimer(t, "pa", "messages", "android:**/x/a.db", "android:**/x/c.db")
			b = globClaimer(t, "pb", "messages", "android:**/x/b.db", "android:**/x/c.db")
		}
		_, err := artparse.New(c, []artparse.Registered{a, b}, opts)
		if err == nil || !strings.Contains(err.Error(), "both claim table") {
			t.Errorf("%s: err %v, want the claim collision", name, err)
		}
	}
}

// Defence in depth: two parsers' claims on one folded table for one artifact are an error, never a pick or a drop.
func TestClaimedTablesOfOneArtifactNeverShareATable(t *testing.T) {
	ok := []artparse.Claimed{{Table: "message", Parser: "a"}, {Table: "calls", Parser: "b"}, {Table: "message", Parser: "a"}}
	if err := artparse.CheckArtifactClaims(ok); err != nil {
		t.Fatalf("one parser, one table twice: %v", err)
	}
	err := artparse.CheckArtifactClaims([]artparse.Claimed{{Table: "message", Parser: "ios.sms"}, {Table: "MESSAGE", Parser: "android.whatsapp.msgstore"}})
	if err == nil || !strings.Contains(err.Error(), "ios.sms") || !strings.Contains(err.Error(), "android.whatsapp.msgstore") {
		t.Fatalf("err %v, want an error naming both parsers", err)
	}
}

// 4I-R23: the claims listed for a backup data file that no Manifest.db could name are those of its own platform. A backup
// is an iOS acquisition, so an Android parser's claims are no claims on it; listing them would report a parser that can
// never have parsed the file.
func TestClaimedTablesOfAnUnnamedBackupFileAreOfItsPlatformOnly(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(bspec{files: stdFiles(), plist: plistYes, manifest: randomBytes(4096)})
	droid := artparse.WithPlatforms(regFake(t, &claimMapper{claimParser{name: "droid-claim", status: parse.Applicable, claims: []string{"wa_rows"}}, []string{"wa_rows"}}, "droid-claim"), parse.PlatformAndroid)
	h := bc.host(nil, regFake(t, smsMapper{}, "ios-sms-claim"), droid)
	got, err := h.ClaimedTables(context.Background(), r.file(smsDomain, smsRel).ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Parser != "ios-sms-claim" || got[0].Table != "message" || !got[0].Unparsed {
		t.Errorf("claims = %+v, want the one Unparsed iOS claim of message and none of the Android parser", got)
	}
}

// extraRoleMapper adds a second input role (with the given globs) to a globClaimMapper.
type extraRoleMapper struct {
	globClaimMapper
	role  string
	globs []string
}

func (p *extraRoleMapper) Meta() parse.Meta {
	m := p.globClaimMapper.Meta()
	m.Inputs = append(m.Inputs, parse.InputSpec{Role: p.role, Globs: p.globs})
	return m
}

// 4I4 final review M1: each side of a claim comparison is judged by its own claim's role. A parser whose claim role has
// other globs than the other parser's must not be compared with the other parser's role, nor the reverse. validateClaims
// forces "db" today, so claimsDisjoint is driven directly with two different roles.
func TestClaimsDisjointUsesEachSidesOwnRole(t *testing.T) {
	a := regFake(t, &extraRoleMapper{globClaimMapper{claimMapper{claimParser{name: "pa", status: parse.Applicable, claims: []string{"t"}}, []string{"t"}}, []string{"android:x/a.db"}}, "alt", []string{"./b.db"}}, "pa")
	b := globClaimer(t, "pb", "t", "android:y/b.db")
	if !artparse.ClaimsDisjoint(a, "db", b, "db") {
		t.Error("a.db vs b.db in the db roles: want disjoint")
	}
	if artparse.ClaimsDisjoint(a, "alt", b, "db") {
		t.Error("a's alt role (b.db) vs b's db role (b.db): the same name, want not disjoint")
	}
	if artparse.ClaimsDisjoint(b, "db", a, "alt") {
		t.Error("the reverse pairing: want not disjoint")
	}
}

// 4I4 final review D5: an empty final segment (meta validation rejects such a glob, so it is driven directly) is
// never decided disjoint.
func TestSegmentsDisjointRefusesAnEmptySegment(t *testing.T) {
	if artparse.SegmentsDisjoint("", "a.db") || artparse.SegmentsDisjoint("a.db", "") {
		t.Error("an empty final segment decided disjoint")
	}
}

// 4I4 final review D5: a backslash or a brace segment (meta validation rejects such globs; driven directly) and a "**"
// final element are never decided disjoint from a literal.
func TestSegmentsDisjointRefusesEscapeAndBraceSyntax(t *testing.T) {
	for _, x := range []string{`a\b.db`, "{a,b}.db", "**"} {
		if artparse.SegmentsDisjoint(x, "c.db") || artparse.SegmentsDisjoint("c.db", x) {
			t.Errorf("segment %q decided disjoint from c.db", x)
		}
	}
}
