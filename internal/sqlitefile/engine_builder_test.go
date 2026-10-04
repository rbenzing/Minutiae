package sqlitefile_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"

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
	// Tables, indexes, overflow chains, WITHOUT ROWID tables, updates, deletes
	// and dropped tables.
	t.Run("tables", runTableScenarios)
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
// counts a trailing partial page as a page (zero-padded); Info.FilePages counts
// it too (TestEngineValidHeaderCountCoveredByPartialPage compares the two), and
// the reader serves only the bytes that are there.
func TestEngineTrailingPartialPage(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.SetHeaderPages(0, true) // so the engine counts from the file
	db := openEngine(t, writeTemp(t, append(b.Bytes(), make([]byte, 100)...)))
	got := pragmaString(t, db, "page_count") == "2"
	if got != enginePartialTrailingPageCounts {
		t.Errorf("engine counts the partial trailing page = %v, recorded constant says %v", got, enginePartialTrailingPageCounts)
	}
}

// engineEncodingForField is what the engine does with a text encoding header
// field outside 1..3, measured by TestEngineTextEncodingHeaderField: the
// builder encoding (1 UTF-8, 2 UTF-16le, 3 UTF-16be) whose text the engine
// reads back intact when the field is patched to the key; 0 means the engine
// reads none of them (it refuses or fails).
var engineEncodingForField = map[uint32]int{4: 1, 5: 1, 6: 2, 7: 3, 0x102: 2}

// TestEngineTextEncodingHeaderField: a database whose header encoding field is
// 4..7 (or has bits above the low two) still holds text in one real encoding.
// For each field the test writes the same text in each of the three encodings
// (schema and rows), patches the field, and asks the engine which one it can
// still read back.
func TestEngineTextEncodingHeaderField(t *testing.T) {
	const text = "héllo, wörld ☃ 𝄞"
	for field, want := range engineEncodingForField {
		var readable []int
		for enc := 1; enc <= 3; enc++ {
			b := sqlitetest.New(sqlitetest.Options{Encoding: enc})
			b.CreateTable("t", "CREATE TABLE t(a)").Insert(1, text)
			b.Patch(56, byte(field>>24), byte(field>>16), byte(field>>8), byte(field))
			db := openEngine(t, writeTemp(t, b.Bytes()))
			var got string
			if err := db.QueryRow("select a from t").Scan(&got); err == nil && got == text {
				readable = append(readable, enc)
			} else {
				t.Logf("field %#x, text written as encoding %d: %v %q", field, enc, err, got)
			}
		}
		got := 0
		if len(readable) == 1 {
			got = readable[0]
		} else if len(readable) > 1 {
			t.Fatalf("field %#x: the engine reads text of several encodings: %v", field, readable)
		}
		if got != want {
			t.Errorf("field %#x: the engine reads the text written as encoding %d, recorded constant says %d", field, got, want)
		}
	}
}

// TestEngineValidHeaderCountCoveredByPartialPage: a 1.5-page file whose valid
// header declares 2 pages is opened by the engine (the missing half page reads
// as zeros), so the reader must not list it in EngineRefuses; 3 declared pages
// are refused. FilePages equals the engine page count of the same file.
func TestEngineValidHeaderCountCoveredByPartialPage(t *testing.T) {
	for _, c := range []struct {
		declared uint32
		refuses  bool
	}{{2, false}, {3, true}} {
		b := sqlitetest.New(sqlitetest.Options{})
		b.SetHeaderPages(c.declared, true)
		data := append(b.Bytes(), make([]byte, 2048)...)
		got, err := engineRefuses(t, data)
		t.Logf("declared %d pages on 1.5 pages: engine refuses = %v (%v)", c.declared, got, err)
		if got != c.refuses {
			t.Errorf("declared %d: engine refuses = %v (%v), want %v", c.declared, got, err, c.refuses)
		}
		db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if listed := len(db.Info().EngineRefuses) > 0; listed != got {
			t.Errorf("declared %d: EngineRefuses %q, engine refuses = %v", c.declared, db.Info().EngineRefuses, got)
		}
	}
	b := sqlitetest.New(sqlitetest.Options{})
	b.SetHeaderPages(0, true)
	data := append(b.Bytes(), make([]byte, 100)...)
	db := openEngine(t, writeTemp(t, data))
	r, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pragmaString(t, db, "page_count") != fmt.Sprint(r.Info().FilePages) {
		t.Errorf("engine page_count %s, FilePages %d", pragmaString(t, db, "page_count"), r.Info().FilePages)
	}
}
