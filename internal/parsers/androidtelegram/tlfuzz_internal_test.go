package androidtelegram

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"maps"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// countBudget is an unbounded budget that counts what is held.
type countBudget struct{ held int64 }

func (b *countBudget) Alloc(n int64) error { b.held += n; return nil }
func (b *countBudget) Free(n int64)        { b.held -= n }

var fuzzTypes = []string{typeMessage, typeUser, typeChat, typeReplies}

// seedsPerType caps the seeds of one fuzzed type per fixture file, so every type gets its own (Task 7 review M2).
const seedsPerType = 10

// FuzzTLDecode (plan 4I4-P13, T3; plan 4I4b Task 7 adds MessageReplies, the object replies_data decodes as): the walker over the Telegram table and arbitrary bytes never panics, ends
// inside the input and never lets a KSecret field hold bytes. Seeds are the TL objects found in the images of
// telegram-v181 and telegram-hostile (each constructor ID of the four types found there, with the bytes after it),
// taken in ascending ID order with at most seedsPerType per type and file: the same seeds on every run (Task 7
// review M2, carried into Task 8 as C6).
func FuzzTLDecode(f *testing.F) {
	// the keys in (ID, type) order: the same seeds on every run (plan 4I4c W1 keys the table by both)
	keys := slices.SortedFunc(maps.Keys(table), func(a, b tl.Key) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Type, b.Type))
	})
	perType := make([]int, len(fuzzTypes))
	for _, name := range []string{"telegram-v181", "telegram-hostile"} {
		fx := parsertest.LoadFixture(f, "testdata", name)
		for _, role := range slices.Sorted(maps.Keys(fx.Files)) {
			raw := fx.Files[role]
			for i, typ := range fuzzTypes {
				seeds := 0
				for _, k := range keys {
					if k.Type != typ || seeds >= seedsPerType {
						continue
					}
					var pat [4]byte
					binary.LittleEndian.PutUint32(pat[:], k.ID)
					if at := bytes.Index(raw, pat[:]); at >= 0 {
						f.Add(raw[at:min(at+512, len(raw))], uint8(i))
						seeds++
					}
				}
				perType[i] += seeds
			}
		}
	}
	for i, n := range perType {
		if n == 0 {
			f.Fatalf("no seed of type %s", fuzzTypes[i])
		}
	}
	// plan 4I4c Task 8: the whole blobs of R83 (inline markup: callback, switch, copy and profile buttons) and R88 (a poll
	// with answers, results and voters) of telegram-v181, as Message: the Stage B surface the walker must contain
	for _, rowid := range []int64{83, 88} {
		f.Add(v181Blob(f, rowid), uint8(0))
	}
	f.Add([]byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, b []byte, which uint8) {
		typ := fuzzTypes[int(which)%len(fuzzTypes)]
		bud := &countBudget{}
		r := tl.Decode(b, typ, table, bud)
		if r.End < 0 || r.End > len(b) {
			t.Fatalf("end %d of %d", r.End, len(b))
		}
		for _, n := range r.Fields {
			if n.Kind == tl.KSecret && n.Bytes != nil {
				t.Fatalf("%s: a secret holds bytes", n.Path)
			}
		}
		bud.Free(r.Charged())
		if bud.held != 0 {
			t.Fatalf("budget still holds %d after freeing Charged", bud.held)
		}
	})
}

// v181Blob is the data blob of one messages_v2 row of telegram-v181, by rowid (a seed of FuzzTLDecode).
func v181Blob(t testing.TB, rowid int64) []byte {
	t.Helper()
	data := parsertest.LoadFixture(t, "testdata", "telegram-v181").Files["db"]
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(data), DBSize: int64(len(data))}, parse.NewBudget(1<<30).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tb, err := db.Table(t.Context(), tableMessagesV2, []string{colData}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	i := tb.Col(colData)
	err = tb.Scan(t.Context(), func(row sqlitedb.Row) error {
		if id, _ := row.Rowid(); id == rowid {
			if b, ok := row.Blob(i); ok {
				out = bytes.Clone(b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("telegram-v181 holds no messages_v2 row %d", rowid)
	}
	return out
}
