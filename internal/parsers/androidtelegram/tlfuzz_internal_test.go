package androidtelegram

import (
	"bytes"
	"encoding/binary"
	"maps"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
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
	ids := slices.Sorted(maps.Keys(table))
	perType := make([]int, len(fuzzTypes))
	for _, name := range []string{"telegram-v181", "telegram-hostile"} {
		fx := parsertest.LoadFixture(f, "testdata", name)
		for _, role := range slices.Sorted(maps.Keys(fx.Files)) {
			raw := fx.Files[role]
			for i, typ := range fuzzTypes {
				seeds := 0
				for _, id := range ids {
					if table[id].Type != typ || seeds >= seedsPerType {
						continue
					}
					var pat [4]byte
					binary.LittleEndian.PutUint32(pat[:], id)
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
