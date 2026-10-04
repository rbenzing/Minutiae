package sqlitefile_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.db")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuilderMatchesEngine: the builder's databases are real databases to the
// independent engine, and the engine reads back the options.
func TestBuilderMatchesEngine(t *testing.T) {
	encName := map[int]string{1: "UTF-8", 2: "UTF-16le", 3: "UTF-16be"}
	for _, o := range []sqlitetest.Options{
		{},
		{PageSize: 512},
		{PageSize: 1024, Reserved: 8},
		{PageSize: 32768},
		{PageSize: 65536},
		{Reserved: 32},
		{Encoding: 2},
		{Encoding: 3},
		{AutoVacuum: 1},
		{AutoVacuum: 2},
		{UserVersion: -42, AppID: 0x4d494e55, ChangeCounter: 12},
	} {
		b := sqlitetest.New(o)
		db := openEngine(t, writeTemp(t, b.Bytes()))
		if got := pragmaString(t, db, "integrity_check"); got != "ok" {
			t.Errorf("%+v: integrity_check = %q", o, got)
		}
		ps := o.PageSize
		if ps == 0 {
			ps = 4096
		}
		enc := o.Encoding
		if enc == 0 {
			enc = 1
		}
		for pragma, want := range map[string]string{
			"page_size":      strconv.Itoa(ps),
			"encoding":       encName[enc],
			"auto_vacuum":    strconv.Itoa(o.AutoVacuum),
			"user_version":   strconv.Itoa(int(o.UserVersion)),
			"application_id": strconv.Itoa(int(int32(o.AppID))),
			"page_count":     "1",
		} {
			if got := pragmaString(t, db, pragma); got != want {
				t.Errorf("%+v: pragma %s = %q, want %q", o, pragma, got, want)
			}
		}
	}
}

// engineRefuses opens data in the engine (a copy) and reports whether it
// refuses to read the schema, with the error.
func engineRefuses(t *testing.T, data []byte) (bool, error) {
	t.Helper()
	db := openEngine(t, writeTemp(t, data))
	var n int
	err := db.QueryRow("select count(*) from sqlite_schema").Scan(&n)
	return err != nil, err
}

// TestEngineRefusesToleratedHeaders validates Info.EngineRefuses against the
// engine, which decides: a header the reader tolerates is listed as refused
// exactly when the engine refuses it.
func TestEngineRefusesToleratedHeaders(t *testing.T) {
	for _, c := range toleratedHeaders {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{})
			c.patch(b)
			got, err := engineRefuses(t, b.Bytes())
			t.Logf("engine refuses = %v (%v)", got, err)
			if got != c.refuses {
				t.Errorf("engine refuses = %v (%v), the reader's table says %v", got, err, c.refuses)
			}
		})
	}
	t.Run("header count beyond the file", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{})
		b.SetHeaderPages(10, true)
		got, err := engineRefuses(t, b.Bytes())
		t.Logf("engine refuses = %v (%v)", got, err)
		if !got {
			t.Error("the engine reads a file whose valid header declares more pages than it holds; Open lists that as refused")
		}
	})
	// Observations that no assertion depends on yet, kept for the report.
	for _, enc := range []byte{4, 5, 6, 7} {
		b := sqlitetest.New(sqlitetest.Options{})
		b.Patch(56, 0, 0, 0, enc)
		got, err := engineRefuses(t, b.Bytes())
		t.Logf("observation: encoding field %d: engine refuses = %v (%v)", enc, got, err)
	}
}

// TestEngineTrailingPartialPage: with no trusted header count the engine
// counts a trailing partial page as a page (zero-padded); Info.FilePages
// counts whole pages only and warns, so the two differ by one here
// (recorded as a limitation in the task report).
func TestEngineTrailingPartialPage(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.SetHeaderPages(0, true) // so the engine counts from the file
	db := openEngine(t, writeTemp(t, append(b.Bytes(), make([]byte, 100)...)))
	got := pragmaString(t, db, "page_count") == "2"
	if got != enginePartialTrailingPageCounts {
		t.Errorf("engine counts the partial trailing page = %v, recorded constant says %v", got, enginePartialTrailingPageCounts)
	}
}
