package decmpfs

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

func value(typ uint32, size uint64, payload []byte) []byte {
	v := make([]byte, HeaderSize, HeaderSize+len(payload))
	copy(v, "fpmc")
	binary.LittleEndian.PutUint32(v[4:], typ)
	binary.LittleEndian.PutUint64(v[8:], size)
	return append(v, payload...)
}

func TestParseHeader(t *testing.T) {
	good, err := ParseHeader(value(4, 70000, []byte("x")))
	if err != nil || good.Type != 4 || good.Size != 70000 {
		t.Fatalf("ParseHeader = %+v, %v", good, err)
	}
	bad := map[string][]byte{
		"empty":        nil,
		"short":        []byte("fpmc-short"),
		"bad magic":    append([]byte("XXXX"), make([]byte, 12)...),
		"size > int64": value(3, math.MaxInt64+1, nil),
		"size max":     value(3, math.MaxUint64, nil),
	}
	for name, b := range bad {
		h, err := ParseHeader(b)
		var he *HeaderError
		if !errors.As(err, &he) || !errors.Is(err, filesys.ErrUnsupported) || h != (Header{}) {
			t.Errorf("%s: ParseHeader = %+v, %v; want a *HeaderError wrapping ErrUnsupported and a zero Header", name, h, err)
		}
	}
	if h, err := ParseHeader(value(3, math.MaxInt64, nil)); err != nil || h.Size != math.MaxInt64 {
		t.Errorf("the largest valid size: %+v, %v", h, err)
	}
}

func TestDecodableIsOnlyThreeAndFour(t *testing.T) {
	for typ := uint32(0); typ < 64; typ++ {
		want := typ == 3 || typ == 4
		if got := (Header{Type: typ}).Decodable(); got != want {
			t.Errorf("type %d: Decodable = %v, want %v", typ, got, want)
		}
	}
	// No guessed layout may ever become decodable by accident of a table edit.
	for _, typ := range []uint32{1, 2, 5, 6, 7, 8, 9, 10, 11, 12, math.MaxUint32} {
		if (Header{Type: typ}).Decodable() {
			t.Errorf("type %d must not be decodable without a real-fixture-backed layout", typ)
		}
	}
}

func TestUnsupportedErrorCarriesTheType(t *testing.T) {
	err := error(&UnsupportedError{Type: 7, Size: 1234, Reason: "lzvn is not decoded"})
	var ue *UnsupportedError
	if !errors.As(err, &ue) || ue.Type != 7 || ue.Size != 1234 || !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("errors.As/Is failed: %v", err)
	}
	if errors.Is(err, filesys.ErrCorrupt) {
		t.Error("an unsupported type is not corruption")
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(value(3, 5, nil))
	f.Add(value(4, 1<<40, nil))
	f.Add([]byte("fpmc"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHeader(b)
		if err == nil && (len(b) < HeaderSize || h.Size > math.MaxInt64) {
			t.Fatalf("accepted %x as %+v", b, h)
		}
	})
}
