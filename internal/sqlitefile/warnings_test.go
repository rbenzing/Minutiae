package sqlitefile_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// goldenWarningCodes is copied from the plan's Format reference ("Warning
// codes", a closed set). Adding a code means changing the reference and this
// slice together.
var goldenWarningCodes = []string{
	"truncated-file", "hdr-fractions", "hdr-encoding-invalid", "hdr-version-bytes", "hdr-counter-mismatch",
	"page-count-clamped", "page-unavailable", "page-type-invalid", "page-range", "cell-pointer", "cell-overflow-chain",
	"cell-too-large", "record-invalid", "record-reserved-serial", "btree-cycle", "btree-depth", "btree-order", "btree-shape", "freelist-cycle",
	"freelist-count", "freelist-leaf-count", "freeblock-chain", "ptrmap-mismatch", "schema-row-invalid",
	"schema-duplicate", "schema-sql-unparsed", "wal-header-invalid", "wal-page-size-mismatch", "wal-torn-tail",
	"wal-mode-mismatch", "wal-frames-not-applied", "wal-page1-mismatch", "live-pages-unavailable", "journal-header-invalid", "journal-sector-invalid", "journal-page-size-mismatch",
	"journal-no-page-size", "journal-page-invalid", "journal-hot", "journal-super-unknown", "journal-and-wal",
	"journal-duplicate-page", "snapshot-unavailable", "owner-changed", "pages-unattributed", "limit-reached", "suppressed",
}

// exportedWarnConstants parses the non-test sources of the package and
// returns the string value of every exported constant whose name starts with
// Warn, so the pin cannot go stale against the code.
func exportedWarnConstants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range parseNonTestFiles(t) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !n.IsExported() || !strings.HasPrefix(n.Name, "Warn") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s must be a plain string literal constant", n.Name)
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[n.Name] = v
				}
			}
		}
	}
	return out
}

func TestWarningCodesPinned(t *testing.T) {
	consts := exportedWarnConstants(t)
	var got []string
	seen := map[string]string{}
	for name, v := range consts {
		if prev, dup := seen[v]; dup {
			t.Errorf("constants %s and %s share the value %q", prev, name, v)
		}
		seen[v] = name
		got = append(got, v)
	}
	want := append([]string(nil), goldenWarningCodes...)
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("exported Warn* constants differ from the Format reference\n got: %q\nwant: %q", got, want)
	}
	if len(want) != 47 {
		t.Errorf("the golden slice has %d codes; the Format reference lists 47 (43 plus wal-frames-not-applied, wal-page1-mismatch and live-pages-unavailable, ruled in Tasks 7 and 8, and pages-unattributed, ruled in Task 10 Q4)", len(want))
	}

	// The collector accepts exactly this set, so a code any later test
	// provokes is in it.
	known := sqlitefile.KnownWarningCodes()
	sort.Strings(known)
	if fmt.Sprint(known) != fmt.Sprint(want) {
		t.Errorf("collector's known codes differ from the pinned set\n got: %q\nwant: %q", known, want)
	}

	w := sqlitefile.NewTestWarnings(0)
	for _, c := range goldenWarningCodes {
		w.Add(sqlitefile.Warning{Code: c, Page: 1})
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an unknown code must panic in test mode")
			}
		}()
		w.Add(sqlitefile.Warning{Code: "not-a-code"})
	}()
}

func TestWarningsDedupAndCap(t *testing.T) {
	w := sqlitefile.NewTestWarnings(0)
	a := sqlitefile.Warning{Code: sqlitefile.WarnPageRange, File: sqlitefile.FileDB, Page: 3, Offset: 100, Msg: "x"}
	w.Add(a)
	w.Add(a)
	w.Add(a)
	if got := w.Snapshot(); len(got) != 1 || got[0] != a {
		t.Fatalf("identical warnings must collapse to one: %+v", got)
	}
	// Different code, file, page or message are distinct.
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageUnavailable, File: sqlitefile.FileDB, Page: 3, Msg: "x"})
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, File: sqlitefile.FileWAL, Page: 3, Msg: "x"})
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, File: sqlitefile.FileDB, Page: 4, Msg: "x"})
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, File: sqlitefile.FileDB, Page: 3, Msg: "y"})
	if got := len(w.Snapshot()); got != 5 {
		t.Fatalf("distinct warnings: got %d, want 5", got)
	}

	// Fill to the cap of 1000 distinct, then overflow.
	w = sqlitefile.NewTestWarnings(0)
	for i := 0; i < 1000; i++ {
		w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, File: sqlitefile.FileDB, Page: uint32(i + 1)})
	}
	if got := len(w.Snapshot()); got != 1000 {
		t.Fatalf("at the cap: %d warnings, want 1000", got)
	}
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, File: sqlitefile.FileDB, Page: 5000})
	snap := w.Snapshot()
	if len(snap) != 1001 || snap[1000].Code != sqlitefile.WarnSuppressed {
		t.Fatalf("the 1001st distinct warning must add exactly one %q line, got %d entries, last %+v", sqlitefile.WarnSuppressed, len(snap), snap[len(snap)-1])
	}
	for i := 0; i < 50; i++ {
		w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, File: sqlitefile.FileDB, Page: uint32(6000 + i)})
	}
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnCellPointer, File: sqlitefile.FileDB, Page: 1}) // duplicate of a kept one
	if got := len(w.Snapshot()); got != 1001 {
		t.Fatalf("past the cap: %d warnings, want 1001 (one suppressed line only)", got)
	}

	// Snapshot is a copy in both directions.
	snap = w.Snapshot()
	snap[0].Msg = "mutated"
	if again := w.Snapshot(); again[0].Msg != "" || len(again) != 1001 {
		t.Fatal("mutating a snapshot changed the collector")
	}
	small := sqlitefile.NewTestWarnings(0)
	small.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, Page: 1})
	before := small.Snapshot()
	small.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, Page: 2})
	if len(before) != 1 {
		t.Fatal("a snapshot grew after it was taken")
	}

	// A smaller explicit cap is honoured too.
	c := sqlitefile.NewTestWarnings(3)
	for i := 0; i < 10; i++ {
		c.Add(sqlitefile.Warning{Code: sqlitefile.WarnPageRange, Page: uint32(i + 1)})
	}
	if got := c.Snapshot(); len(got) != 4 || got[3].Code != sqlitefile.WarnSuppressed {
		t.Fatalf("cap 3: %+v", got)
	}
}

func TestWarningsConcurrent(t *testing.T) {
	w := sqlitefile.NewTestWarnings(0)
	const workers, per = 8, 200
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				// Every worker adds the same 200 distinct warnings.
				w.Add(sqlitefile.Warning{Code: sqlitefile.WarnRecordInvalid, File: sqlitefile.FileDB, Page: uint32(i + 1), Msg: "m"})
				if i%17 == 0 {
					_ = w.Snapshot()
				}
			}
		}()
	}
	wg.Wait()
	if got := len(w.Snapshot()); got != per {
		t.Fatalf("got %d warnings, want %d", got, per)
	}
}

func TestWarningFileKindsAreCarried(t *testing.T) {
	w := sqlitefile.NewTestWarnings(0)
	w.Add(sqlitefile.Warning{Code: sqlitefile.WarnWALTornTail, File: sqlitefile.FileWAL, Page: 0, Offset: 32, Msg: "torn"})
	got := w.Snapshot()
	if len(got) != 1 || got[0].File != sqlitefile.FileWAL || got[0].Offset != 32 || got[0].Msg != "torn" {
		t.Fatalf("%+v", got)
	}
}
