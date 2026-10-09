package sqlitedb_test

// Guards of the decoder's package surface: history is never part of a live
// scan, and the package can neither write nor open files.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// nonTestSources parses the package's non-test Go files.
func nonTestSources(t testing.TB) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = f
	}
	if len(out) < 8 {
		t.Fatalf("only %d source files found: the scan would be vacuous", len(out))
	}
	return out
}

// selectorsNamed returns the selector expressions (x.Name) of f whose name is
// in names. Comments and strings are not code and never match.
func selectorsNamed(f *ast.File, names ...string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok && slices.Contains(names, s.Sel.Name) {
			out = append(out, s.Sel.Name)
		}
		return true
	})
	return out
}

func parseSnippet(t testing.TB, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestSqlitedbNeverUsesAsFoundOrHistory: the live scan is the library's Live()
// view only. The raw view (AsFound) and the recovery pass (History) are not
// available to the decoder's own code; recovered rows enter only through
// FromRecovered, from a caller.
func TestSqlitedbNeverUsesAsFoundOrHistory(t *testing.T) {
	banned := []string{"AsFound", "History"}
	t.Run("self-test", func(t *testing.T) {
		bad := parseSnippet(t, "package x\nfunc f(d *D) { _ = d.AsFound(); _ = d.History(); _ = d.Live() }\ntype D struct{}")
		if got := selectorsNamed(bad, banned...); len(got) != 2 {
			t.Fatalf("the scan found %v in a snippet with both calls, want 2", got)
		}
		ok := parseSnippet(t, "package x\n// d.AsFound() d.History()\nfunc f(d *D) { _ = d.Live(); _ = \"d.History()\" }\ntype D struct{}")
		if got := selectorsNamed(ok, banned...); len(got) != 0 {
			t.Fatalf("the scan flagged %v in comments, strings and Live()", got)
		}
	})
	for name, f := range nonTestSources(t) {
		if got := selectorsNamed(f, banned...); len(got) != 0 {
			t.Errorf("%s uses %v: history and the raw view must never reach the live decoder", name, got)
		}
	}
}

var bannedImports = []string{"os", "io/ioutil", "syscall", "os/exec", "path/filepath", "path", "net", "net/http", "database/sql", "unsafe"}

// WriteString is not listed: strings.Builder is memory (Row.Locator).
var writeNames = []string{"Write", "WriteAt", "WriteFile", "Truncate", "Seek", "Sync", "Create", "OpenFile", "Remove", "RemoveAll", "Rename", "Mkdir", "MkdirAll", "Chmod"}

func writeOrFileUses(f *ast.File) []string {
	var out []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if slices.Contains(bannedImports, p) {
			out = append(out, "import "+p)
		}
	}
	out = append(out, selectorsNamed(f, writeNames...)...)
	// An exported declaration whose name promises a write.
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.IsExported() {
			for _, w := range []string{"Write", "Create", "Delete", "Update", "Insert", "Save", "Store", "Truncate"} {
				if strings.HasPrefix(fd.Name.Name, w) {
					out = append(out, "func "+fd.Name.Name)
				}
			}
		}
	}
	return out
}

// TestSqlitedbHasNoWriteOrFileAPI: the decoder works on io.ReaderAt values the
// host hands it. It imports nothing that opens or writes a file and exports no
// function that writes.
func TestSqlitedbHasNoWriteOrFileAPI(t *testing.T) {
	t.Run("self-test", func(t *testing.T) {
		bad := parseSnippet(t, "package x\nimport \"os\"\nfunc WriteRow(f *F) { f.Truncate(0); f.Seek(0, 0) }\ntype F struct{}")
		if got := writeOrFileUses(bad); len(got) != 4 {
			t.Fatalf("found %v in a snippet with an os import, a Write function, Truncate and Seek; want 4", got)
		}
		ok := parseSnippet(t, "package x\nimport \"io\"\nfunc Scan(r io.ReaderAt) {}\n")
		if got := writeOrFileUses(ok); len(got) != 0 {
			t.Fatalf("flagged %v in a read-only snippet", got)
		}
	})
	for name, f := range nonTestSources(t) {
		if got := writeOrFileUses(f); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// TestHistoryRowsNeverReachScan: on files that hold deleted, superseded and
// rolled-back rows (the library's History lists them), a scan delivers exactly
// the live rows of the oracle, none of them recovered, while the rows of the
// history become recovered Rows only through FromRecovered and keep the
// recovered invariants.
func TestHistoryRowsNeverReachScan(t *testing.T) {
	for _, name := range []string{"freelist-dropped", "persist-journal", "hot-journal", "wal-uncheckpointed", "builder-journal-multiseg", "secure-delete"} {
		t.Run(name, func(t *testing.T) {
			db, wal, journal := loadFixture(t, name)
			exp := loadExpect(t, name)
			d := openBytes(t, db, wal, journal, bigBudget())

			tables, err := d.Tables(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range tables {
				tb := mustTable(t, d, n)
				_ = tb.Scan(t.Context(), func(r sqlitedb.Row) error {
					if r.Recovered() != nil {
						t.Errorf("%s: a scanned row is recovered", n)
					}
					checkRowInvariants(t, r)
					return nil
				})
			}
			compareWithOracle(t, exp, d)

			lib, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if wal != nil {
				if _, err := lib.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
					t.Fatal(err)
				}
			}
			if journal != nil {
				if _, err := lib.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
					t.Fatal(err)
				}
			}
			history, built := 0, 0
			if _, err := lib.History().Rows(t.Context(), func(rr sqlitefile.RecoveredRow) bool {
				history++
				if rr.Index != "" || rr.Table == "" {
					return true
				}
				tb, err := d.Table(t.Context(), rr.Table, nil, nil)
				if err != nil {
					return true
				}
				r, err := sqlitedb.FromRecovered(tb, rr)
				if err != nil {
					t.Errorf("FromRecovered(%s): %v", rr.Table, err)
					return true
				}
				checkRecoveredInvariants(t, r)
				built++
				r.Release()
				return true
			}); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d history rows, %d built into Rows", name, history, built)
			if name != "secure-delete" && history == 0 {
				t.Error("the library's history holds no row: the guard would be vacuous on this fixture")
			}
		})
	}
}
