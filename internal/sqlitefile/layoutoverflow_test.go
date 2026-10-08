package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// varint9 encodes v as the nine-byte SQLite varint (v needs more than 56 bits).
func varint9(v uint64) []byte {
	b := make([]byte, 9)
	hi := v >> 8
	for i := range 8 {
		b[i] = 0x80 | byte(hi>>(49-7*uint(i)))&0x7f
	}
	b[8] = byte(v)
	return b
}

// TestLayoutDeclaredPayloadNearMaxInt64: the page count an overflow chain
// needs is computed without overflowing, so a cell that declares a payload
// within a page of MaxInt64 still claims its chain (final review A, F6). Here
// the chain's head is also the freelist's trunk: the second claim is a Problem
// (or the chain is warned about), never silence.
func TestLayoutDeclaredPayloadNearMaxInt64(t *testing.T) {
	const ps = 4096
	for _, payload := range []uint64{math.MaxInt64, math.MaxInt64 - 40000, 1 << 62} {
		b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
		tb := b.CreateTable("t", "create table t(a)")
		tb.Insert(1, "x")
		pad := b.CreateTable("pad", "create table pad(z)")
		pad.Insert(1, "y")
		b.DropTable("pad") // its page 3 is now the freelist trunk
		data := b.Bytes()
		leaf := pageAt(data, ps, tb.Root())
		local, spills := sqlitefile.LocalPayload(ps, sqlitefile.PageTableLeaf, int64(payload))
		if !spills {
			t.Fatal("the payload does not spill")
		}
		cell := slices.Concat(varint9(payload), []byte{1}, make([]byte, local), binary.BigEndian.AppendUint32(nil, 3))
		if n, k := sqlitefile.GetVarint(cell); n != payload || k != 9 {
			t.Fatalf("varint9 = %d (%d bytes)", n, k)
		}
		off := ps - len(cell)
		if off < 8+2*2 {
			t.Fatalf("a %d byte cell does not fit the page", len(cell))
		}
		copy(leaf[off:], cell)
		binary.BigEndian.PutUint16(leaf[5:], uint16(off))
		binary.BigEndian.PutUint16(leaf[8:], uint16(off))
		_, v := openLive(t, data, sqlitefile.Options{})
		lay, err := v.Layout(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for _, w := range v.Warnings() {
			warned = warned || w.Code == sqlitefile.WarnCellOverflowChain
		}
		if len(lay.Problems) == 0 && !warned {
			t.Errorf("payload %d: page 3 is claimed by the chain and by the freelist, yet no Problem and no warning (class %v)", payload, lay.Class[3])
		}
	}
}
