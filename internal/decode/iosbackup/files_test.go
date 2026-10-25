package iosbackup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	filesDDL      = "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)"
	filesDDLNoPK  = "CREATE TABLE Files (fileID TEXT, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)"
	filesDDLNoRel = "CREATE TABLE Files (fileID TEXT, domain TEXT, flags INTEGER, file BLOB)"
)

// frow is one Files row; the cells are any so a test can store a BLOB or NULL.
type frow struct {
	id, domain, rel, flags any
	noRelCol               bool
}

func fid(n int) string { return fmt.Sprintf("%040x", n) }

func fileRow(n int, domain, rel string, flags int64) frow {
	return frow{id: fid(n), domain: domain, rel: rel, flags: flags}
}

func manifestBuilder(ddl string, rows ...frow) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("Files", ddl)
	for i, r := range rows {
		if r.noRelCol {
			tb.Insert(int64(i+1), r.id, r.domain, r.flags, nil)
			continue
		}
		tb.Insert(int64(i+1), r.id, r.domain, r.rel, r.flags, nil)
	}
	return b
}

// manifestDB builds a Manifest.db with the Files table of the Task 4 snapshot.
func manifestDB(t testing.TB, rows ...frow) []byte {
	t.Helper()
	return manifestBuilder(filesDDL, rows...).Bytes()
}

func filesOf(db []byte) sqlitedb.Files {
	return sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}
}

func budget() *parse.BudgetView { return parse.NewBudget(1 << 30).View() }

func wantIDs(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, w := range ids {
			if w == id {
				return true
			}
		}
		return false
	}
}

func read(t *testing.T, db []byte, want func(string) bool) *iosbackup.Files {
	t.Helper()
	f, err := iosbackup.ReadFiles(t.Context(), filesOf(db), budget(), want, iosbackup.Options{})
	if err != nil {
		t.Fatalf("ReadFiles: %v", err)
	}
	return f
}

func TestReadFilesMapsWantedFiles(t *testing.T) {
	db := manifestDB(t,
		fileRow(1, "HomeDomain", "Library/SMS/sms.db", 1),
		fileRow(2, "HomeDomain", "Library/SMS", 2),
		fileRow(3, "HomeDomain", "Library/link", 4),
		fileRow(4, "HomeDomain", "Library/unwanted.db", 1),
	)
	f := read(t, db, wantIDs(fid(1), fid(2), fid(3), fid(9)))

	e, st, why := f.Lookup(fid(1))
	if st != iosbackup.Mapped || e != (iosbackup.Entry{Domain: "HomeDomain", RelativePath: "Library/SMS/sms.db", Flags: 1}) || why != "" {
		t.Errorf("sms.db = %+v %v %q", e, st, why)
	}
	if _, st, why = f.Lookup(fid(2)); st != iosbackup.NotAFile || !strings.Contains(why, "flags 2") {
		t.Errorf("directory = %v %q, want NotAFile naming flags 2", st, why)
	}
	if _, st, why = f.Lookup(fid(3)); st != iosbackup.NotAFile || !strings.Contains(why, "flags 4") {
		t.Errorf("symlink = %v %q", st, why)
	}
	if _, st, why = f.Lookup(fid(9)); st != iosbackup.NotListed || why == "" {
		t.Errorf("never inserted = %v %q, want NotListed with a reason", st, why)
	}
	if got := f.Rows(); got != 4 {
		t.Errorf("Rows = %d, want 4", got)
	}
	if got := iosbackup.Kept(f); got != 3 {
		t.Errorf("Kept = %d, want 3 (the unwanted row is not retained)", got)
	}
}

func TestLookupOfAnIDNobodyAskedForIsUndecided(t *testing.T) {
	db := manifestDB(t, fileRow(1, "HomeDomain", "a", 1), fileRow(4, "HomeDomain", "b", 1))
	f := read(t, db, wantIDs(fid(1)))
	// fid(4) is in the table but was not requested, so it was not kept: "not listed" would be false.
	if _, st, why := f.Lookup(fid(4)); st != iosbackup.Undecided || why == "" {
		t.Errorf("unrequested ID = %v %q, want Undecided with a reason", st, why)
	}
}

func TestReadFilesDuplicateFileIDIsConflict(t *testing.T) {
	b := manifestBuilder(filesDDLNoPK,
		fileRow(1, "HomeDomain", "a/one", 1),
		fileRow(1, "HomeDomain", "a/two", 1),
		fileRow(2, "HomeDomain", "b", 1),
	)
	f := read(t, b.Bytes(), wantIDs(fid(1), fid(2)))
	if _, st, why := f.Lookup(fid(1)); st != iosbackup.Duplicate || why == "" {
		t.Errorf("twice listed = %v %q, want Duplicate", st, why)
	}
	if _, st, _ := f.Lookup(fid(2)); st != iosbackup.Mapped {
		t.Errorf("once listed = %v, want Mapped", st)
	}
}

func TestReadFilesUnreadableRowMakesAbsenceUndecided(t *testing.T) {
	t.Run("blob fileID", func(t *testing.T) {
		db := manifestDB(t,
			fileRow(1, "HomeDomain", "a", 1),
			frow{id: []byte{1, 2, 3}, domain: "HomeDomain", rel: "x", flags: int64(1)},
		)
		f := read(t, db, wantIDs(fid(1), fid(7)))
		if _, st, _ := f.Lookup(fid(1)); st != iosbackup.Mapped {
			t.Errorf("present = %v, want Mapped", st)
		}
		if _, st, why := f.Lookup(fid(7)); st != iosbackup.Undecided || why == "" {
			t.Errorf("absent = %v %q, want Undecided", st, why)
		}
	})
	t.Run("null fileID", func(t *testing.T) {
		db := manifestDB(t, frow{id: nil, domain: "HomeDomain", rel: "x", flags: int64(1)})
		f := read(t, db, wantIDs(fid(7)))
		if _, st, _ := f.Lookup(fid(7)); st != iosbackup.Undecided {
			t.Errorf("absent = %v, want Undecided", st)
		}
	})
	t.Run("zeroed leaf page", func(t *testing.T) {
		var rows []frow
		for i := 1; i <= 400; i++ {
			rows = append(rows, fileRow(i, "HomeDomain", fmt.Sprintf("Library/Folder%04d/file-with-a-long-name-%04d.db", i, i), 1))
		}
		db := manifestDB(t, rows...)
		// Page 1 is the schema, page 2 the Files root; zero a leaf below it.
		const ps = 4096
		clear(db[3*ps : 4*ps])
		f := read(t, db, wantIDs(fid(1), fid(401)))
		if _, st, why := f.Lookup(fid(401)); st != iosbackup.Undecided || why == "" {
			t.Errorf("absent ID after a damaged leaf = %v %q, want Undecided, never NotListed", st, why)
		}
	})
}

func TestReadFilesBadEntries(t *testing.T) {
	cases := []struct {
		name        string
		domain, rel string
		field       string
	}{
		{"empty domain", "", "a", "domain"},
		{"slash in domain", "Home/Domain", "a", "domain"},
		{"dotdot domain", "..", "a", "domain"},
		{"dot domain", ".", "a", "domain"},
		{"backslash domain", `Home\Domain`, "a", "domain"},
		{"nul domain", "Home\x00Domain", "a", "domain"},
		{"long domain", strings.Repeat("d", 256), "a", "domain"},
		{"invalid utf8 domain", "Home\xffDomain", "a", "domain"},
		{"empty path", "HomeDomain", "", "relativePath is not canonical"},
		{"dotdot element", "HomeDomain", "a/../b", "relativePath is not canonical"},
		{"dot element", "HomeDomain", "a/./b", "relativePath is not canonical"},
		{"double slash", "HomeDomain", "a//b", "relativePath is not canonical"},
		{"leading slash", "HomeDomain", "/a", "relativePath is not canonical"},
		{"trailing slash", "HomeDomain", "a/", "relativePath is not canonical"},
		{"nul path", "HomeDomain", "a\x00b", "relativePath is not canonical"},
		{"backslash path", "HomeDomain", `a\b`, "relativePath is not canonical"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := manifestDB(t, fileRow(i+1, c.domain, c.rel, 1))
			f := read(t, db, wantIDs(fid(i+1)))
			e, st, why := f.Lookup(fid(i + 1))
			if st != iosbackup.BadEntry || !strings.Contains(why, c.field) {
				t.Errorf("%q %q = %+v %v %q, want BadEntry naming %q", c.domain, c.rel, e, st, why, c.field)
			}
		})
	}
	t.Run("good ones are Mapped", func(t *testing.T) {
		db := manifestDB(t, fileRow(1, "AppDomain-com.example.app", "Library/Preferences/a.plist", 1), fileRow(2, "a.b", "..hidden/.x", 1))
		f := read(t, db, wantIDs(fid(1), fid(2)))
		for _, n := range []int{1, 2} {
			if _, st, why := f.Lookup(fid(n)); st != iosbackup.Mapped {
				t.Errorf("row %d = %v %q, want Mapped", n, st, why)
			}
		}
	})
	t.Run("null cells", func(t *testing.T) {
		db := manifestDB(t,
			frow{id: fid(1), domain: nil, rel: "a", flags: int64(1)},
			frow{id: fid(2), domain: "HomeDomain", rel: nil, flags: int64(1)},
			frow{id: fid(3), domain: "HomeDomain", rel: "a", flags: nil},
		)
		f := read(t, db, wantIDs(fid(1), fid(2), fid(3)))
		for _, n := range []int{1, 2} {
			if _, st, _ := f.Lookup(fid(n)); st != iosbackup.BadEntry {
				t.Errorf("row %d with a NULL text cell = %v, want BadEntry", n, st)
			}
		}
		if _, st, _ := f.Lookup(fid(3)); st != iosbackup.Undecided {
			t.Errorf("NULL flags = %v, want Undecided (never \"not a file\")", st)
		}
	})
}

// readerFunc is a database file that records being touched and fails every read.
type readerFunc func()

func (r readerFunc) ReadAt([]byte, int64) (int, error) {
	r()
	return 0, errors.New("read")
}

func TestReadFilesRowCap(t *testing.T) {
	four := manifestDB(t, fileRow(1, "D", "a", 1), fileRow(2, "D", "b", 1), fileRow(3, "D", "c", 1), fileRow(4, "D", "d", 1))
	if _, err := iosbackup.ReadFiles(t.Context(), filesOf(four), budget(), wantIDs(fid(1)), iosbackup.Options{MaxRows: 3}); !errors.Is(err, iosbackup.ErrTooManyRows) {
		t.Errorf("4 rows over MaxRows 3: err = %v, want ErrTooManyRows", err)
	}
	three := manifestDB(t, fileRow(1, "D", "a", 1), fileRow(2, "D", "b", 1), fileRow(3, "D", "c", 1))
	f, err := iosbackup.ReadFiles(t.Context(), filesOf(three), budget(), wantIDs(fid(1)), iosbackup.Options{MaxRows: 3})
	if err != nil || f == nil || f.Rows() != 3 {
		t.Errorf("exactly 3 rows: %v %v", f, err)
	}
	for _, n := range []int{iosbackup.MaxFilesRows + 1, -1} {
		touched := false
		files := sqlitedb.Files{DB: readerFunc(func() { touched = true }), DBSize: 4096}
		if _, err := iosbackup.ReadFiles(t.Context(), files, budget(), wantIDs(fid(1)), iosbackup.Options{MaxRows: n}); !errors.Is(err, iosbackup.ErrBadOptions) {
			t.Errorf("MaxRows %d: err = %v, want ErrBadOptions", n, err)
		}
		if touched {
			t.Errorf("MaxRows %d: the database was read before the option was refused", n)
		}
	}
}

func TestReadFilesSchemaErrors(t *testing.T) {
	t.Run("no Files table", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{})
		b.CreateTable("Other", "CREATE TABLE Other (a INTEGER)")
		_, err := iosbackup.ReadFiles(t.Context(), filesOf(b.Bytes()), budget(), wantIDs(), iosbackup.Options{})
		if !errors.Is(err, sqlitedb.ErrNoSuchTable) {
			t.Errorf("err = %v, want ErrNoSuchTable", err)
		}
	})
	t.Run("no relativePath column", func(t *testing.T) {
		b := manifestBuilder(filesDDLNoRel, frow{id: fid(1), domain: "D", flags: int64(1), noRelCol: true})
		_, err := iosbackup.ReadFiles(t.Context(), filesOf(b.Bytes()), budget(), wantIDs(), iosbackup.Options{})
		var u *sqlitedb.UnsupportedSchemaError
		if !errors.As(err, &u) || len(u.Missing) != 1 || u.Missing[0] != "relativePath" {
			t.Errorf("err = %v, want UnsupportedSchemaError missing [relativePath]", err)
		}
	})
	t.Run("random bytes", func(t *testing.T) {
		rnd := make([]byte, 4096)
		x := uint32(12345)
		for i := range rnd {
			x = x*1664525 + 1013904223
			rnd[i] = byte(x >> 24)
		}
		_, err := iosbackup.ReadFiles(t.Context(), filesOf(rnd), budget(), wantIDs(), iosbackup.Options{})
		if !errors.Is(err, sqlitedb.ErrEncrypted) {
			t.Errorf("err = %v, want ErrEncrypted", err)
		}
	})
	t.Run("ascii text", func(t *testing.T) {
		txt := bytes.Repeat([]byte("this is plain text, not a database. "), 150)[:4096]
		_, err := iosbackup.ReadFiles(t.Context(), filesOf(txt), budget(), wantIDs(), iosbackup.Options{})
		if !errors.Is(err, sqlitedb.ErrNotSQLite) {
			t.Errorf("err = %v, want ErrNotSQLite", err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		touched := false
		files := sqlitedb.Files{DB: readerFunc(func() { touched = true }), DBSize: iosbackup.MaxManifestBytes + 1}
		_, err := iosbackup.ReadFiles(t.Context(), files, budget(), wantIDs(), iosbackup.Options{})
		if !errors.Is(err, iosbackup.ErrTooLarge) || touched {
			t.Errorf("err = %v touched=%v, want ErrTooLarge before any read", err, touched)
		}
	})
}

func TestReadFilesHonoursTheWAL(t *testing.T) {
	b := manifestBuilder(filesDDL, fileRow(1, "HomeDomain", "base", 1))
	db := b.Bytes()
	snap := b.Snapshot()
	w := b.NewWAL(false, 0x11, 0x22, 0)
	b.Object("Files").Insert(2, fid(2), "HomeDomain", "only/in/wal", int64(1), nil)
	b.CommitTo(w, snap)

	files := filesOf(db)
	files.WAL, files.WALSize = bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))
	f, err := iosbackup.ReadFiles(t.Context(), files, budget(), wantIDs(fid(1), fid(2)), iosbackup.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e, st, _ := f.Lookup(fid(2))
	if st != iosbackup.Mapped || e.RelativePath != "only/in/wal" {
		t.Errorf("WAL-only row = %+v %v, want Mapped", e, st)
	}
	// Without the WAL the row is absent: the test would pass vacuously otherwise.
	g := read(t, db, wantIDs(fid(2)))
	if _, st, _ := g.Lookup(fid(2)); st != iosbackup.NotListed {
		t.Errorf("without the WAL: %v, want NotListed", st)
	}
}

func TestReadFilesHonoursCancelAndBudget(t *testing.T) {
	db := manifestDB(t, fileRow(1, "D", "a", 1))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if f, err := iosbackup.ReadFiles(ctx, filesOf(db), budget(), wantIDs(fid(1)), iosbackup.Options{}); !errors.Is(err, context.Canceled) || f != nil {
		t.Errorf("cancelled: %v %v", f, err)
	}
	small := parse.NewBudget(64).View()
	if f, err := iosbackup.ReadFiles(t.Context(), filesOf(db), small, wantIDs(fid(1)), iosbackup.Options{}); !errors.Is(err, parse.ErrBudget) || f != nil {
		t.Errorf("64-byte budget: %v %v, want a budget error and nothing", f, err)
	}
}

func TestReadFilesChargesKeptEntriesAndReleaseGivesThemBack(t *testing.T) {
	db := manifestDB(t, fileRow(1, "HomeDomain", "a", 1), fileRow(2, "HomeDomain", "b", 1))
	v := parse.NewBudget(1 << 30).View()
	f, err := iosbackup.ReadFiles(t.Context(), filesOf(db), v, wantIDs(fid(1), fid(2)), iosbackup.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Used() < 2*64 {
		t.Errorf("used = %d, want at least 64 per kept entry", v.Used())
	}
	f.Release()
	f.Release()
	if v.Used() != 0 {
		t.Errorf("used after Release = %d, want 0", v.Used())
	}
}

func TestReadFilesKeptEntryBudgetFailureLeavesNothingCharged(t *testing.T) {
	var rows []frow
	var ids []string
	for i := 1; i <= 200; i++ {
		rows = append(rows, fileRow(i, "HomeDomain", fmt.Sprintf("p/%d", i), 1))
		ids = append(ids, fid(i))
	}
	db := manifestDB(t, rows...)
	// A budget only 1 KiB above what the empty read needs cannot hold 200 entries of 64+ bytes.
	tight := parse.NewBudget(readPeak(t, db) + 1024)
	tv := tight.View()
	if _, err := iosbackup.ReadFiles(t.Context(), filesOf(db), tv, wantIDs(ids...), iosbackup.Options{}); !errors.Is(err, parse.ErrBudget) {
		t.Fatalf("err = %v, want a budget error", err)
	}
	if tv.Used() != 0 {
		t.Errorf("used after a failed read = %d, want 0", tv.Used())
	}
}

// readPeak is the budget a read of db needs while keeping nothing.
func readPeak(t *testing.T, db []byte) int64 {
	t.Helper()
	for limit := int64(1 << 10); limit <= 1<<30; limit *= 2 {
		v := parse.NewBudget(limit).View()
		f, err := iosbackup.ReadFiles(t.Context(), filesOf(db), v, wantIDs(), iosbackup.Options{})
		if err == nil {
			f.Release()
			return limit
		}
	}
	t.Fatal("no budget suffices")
	return 0
}
