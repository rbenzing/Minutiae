package sqlitefile_test

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestRecordLengthMismatchIsFlagged: once the record header is used up the
// engine requires the body to end exactly at the payload's end (otherwise
// SQLITE_CORRUPT on a full-row read). A record with surplus payload bytes, or
// whose header holds no columns, is delivered (its values are readable) but
// never silently: one record-length-mismatch warning at the cell (final
// review A, F2). The engine decides each case; a record it reads is not
// flagged.
func TestRecordLengthMismatchIsFlagged(t *testing.T) {
	cases := []struct {
		name    string
		serials []uint64
		body    []byte
	}{
		{"null plus a stray byte", []uint64{0}, []byte{0}},
		{"null plus two stray bytes", []uint64{0}, []byte{0, 0}},
		{"int8 plus a stray byte", []uint64{1}, []byte{5, 0}},
		{"two columns plus stray", []uint64{1, 9}, []byte{5, 0, 0}},
		{"header only", nil, nil},
		{"no columns plus stray", nil, []byte{7}},
		{"exact one column", []uint64{1}, []byte{5}},
		{"exact two columns", []uint64{0, 9}, nil},
		{"more columns than the table", []uint64{1, 9, 8, 0}, []byte{5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{})
			tb := b.CreateTable("t", "CREATE TABLE t(a, b)")
			tb.InsertRaw(1, c.serials, c.body)
			tb.Insert(2, "ok", int64(2))
			data := b.Bytes()

			db := openEngine(t, writeTemp(t, data))
			var a, bb any
			engineErr := db.QueryRow("select a, b from t where rowid = 1").Scan(&a, &bb)

			_, v := openLive(t, data, sqlitefile.Options{})
			rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
			if len(rows) != 2 {
				t.Fatalf("%d rows: the row must still be delivered", len(rows))
			}
			var scanWarn int
			for _, w := range v.Warnings() {
				if w.Code == sqlitefile.WarnRecordLengthMismatch {
					scanWarn++
				}
			}
			t.Logf("engine error: %v", engineErr)
			if want := engineErr != nil; (scanWarn == 1) != want || scanWarn > 1 {
				t.Errorf("engine corrupt=%v but %d record-length-mismatch warnings: %v", want, scanWarn, v.Warnings())
			}
			// A point lookup says the same.
			_, v2 := openLive(t, data, sqlitefile.Options{})
			if _, ok, err := v2.LookupRowid(context.Background(), tb.Root(), 1); !ok || err != nil {
				t.Fatalf("lookup: %v %v", ok, err)
			}
			n := 0
			for _, w := range v2.Warnings() {
				if w.Code == sqlitefile.WarnRecordLengthMismatch {
					n++
				}
			}
			if (n == 1) != (engineErr != nil) {
				t.Errorf("lookup: %d warnings, engine err %v", n, engineErr)
			}
		})
	}
}

// TestDecodeRecordFlagsLengthMismatch: the pure decoder flags the same cases.
func TestDecodeRecordFlagsLengthMismatch(t *testing.T) {
	for _, c := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"exact", []byte{0x02, 0x01, 0x05}, false},
		{"two nulls", []byte{0x03, 0x00, 0x00}, false},
		{"stray byte", []byte{0x02, 0x00, 0x00}, true},
		{"header only", []byte{0x01}, true},
		{"truncated is not a mismatch", []byte{0x02, 0x04}, false},
	} {
		rec, err := sqlitefile.DecodeRecord(c.b, sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if rec.LengthMismatch != c.want {
			t.Errorf("%s: LengthMismatch = %v, want %v", c.name, rec.LengthMismatch, c.want)
		}
	}
}
