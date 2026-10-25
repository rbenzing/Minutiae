package iosbackup_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// hostileOK are the typed refusals a hostile Manifest.db may produce. sqlitedb.ErrInternal (a
// recovered panic) is deliberately not among them.
var hostileOK = []error{
	iosbackup.ErrTooLarge, iosbackup.ErrTooManyRows, sqlitedb.ErrNotSQLite, sqlitedb.ErrEncrypted,
	sqlitedb.ErrCorrupt, sqlitedb.ErrLiveUnavailable, sqlitedb.ErrEngineRefuses, sqlitedb.ErrNoSuchTable,
	sqlitedb.ErrUnsupportedSchema, sqlitedb.ErrUnsupportedCollation, sqlitedb.ErrBadFiles, parse.ErrBudget,
}

func FuzzReadFiles(f *testing.F) {
	f.Add(manifestDB(f, fileRow(1, "HomeDomain", "Library/SMS/sms.db", 1), fileRow(2, "HomeDomain", "Library/SMS", 2)))
	f.Add(manifestDB(f, fileRow(1, "", "a//b", 1), fileRow(2, "Home/Domain", "/a", 4)))
	f.Add(manifestDB(f, frow{id: []byte{1}, domain: nil, rel: nil, flags: nil}))
	f.Add(manifestBuilder(filesDDLNoPK, fileRow(1, "D", "a", 1), fileRow(1, "D", "b", 1)).Bytes())
	f.Add(bytes.Repeat([]byte{0x41}, 4096))
	f.Fuzz(func(t *testing.T, db []byte) {
		if len(db) > 64<<10 {
			t.Skip()
		}
		want := func(id string) bool { return id[len(id)-1]%2 == 1 || id == fid(1) }
		got, err := iosbackup.ReadFiles(t.Context(), filesOf(db), parse.NewBudget(64<<20).View(), want, iosbackup.Options{MaxRows: 1000})
		if err != nil {
			for _, ok := range hostileOK {
				if errors.Is(err, ok) {
					return
				}
			}
			t.Fatalf("untyped error: %v", err)
		}
		for _, id := range []string{fid(1), fid(2), "x"} {
			if _, st, why := got.Lookup(id); st < iosbackup.Mapped || st > iosbackup.BadEntry || (st != iosbackup.Mapped && why == "") {
				t.Fatalf("Lookup(%q) = %v %q", id, st, why)
			}
		}
		got.Release()
	})
}

func FuzzReadEncryption(f *testing.F) {
	for _, s := range plistSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got, why := iosbackup.ReadEncryption(b, parse.NewBudget(64<<20).View())
		if got != iosbackup.EncUnknown && why != "" {
			t.Fatalf("decided %v with a reason %q", got, why)
		}
		if got == iosbackup.EncUnknown && why == "" {
			t.Fatal("unknown without a reason")
		}
	})
}
