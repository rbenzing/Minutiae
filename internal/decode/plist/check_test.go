package plist

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	howett "howett.net/plist"
)

func wantKind(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted")
	}
	if errors.Is(err, ErrInternal) {
		t.Fatalf("reached the recover guard: %v", err)
	}
	if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrLimit) {
		t.Fatalf("error wraps neither ErrMalformed nor ErrLimit: %v", err)
	}
}

func TestCheckRejectsPlistBombs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n, fan  int
		wantErr bool
	}{
		{"exp21", 21, 2, true},
		{"exp40", 40, 2, true},
		{"deep70", 70, 1, true},
		{"ok-exp10", 10, 2, false},
		{"ok-deep30", 30, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			err := CheckBinary(nestedPlist(tc.n, tc.fan), DefaultLimits())
			if time.Since(start) > time.Second {
				t.Fatalf("check took %v", time.Since(start))
			}
			if tc.wantErr {
				wantKind(t, err)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckRejectsOverflowingCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  []byte
	}{
		{"dict-2^63", bigCount(0xD, 1<<63)},
		{"dict-2^63+1", bigCount(0xD, 1<<63+1)},
		{"utf16-2^63+1", bigCount(0x6, 1<<63+1)},
		{"array-2^63", bigCount(0xA, 1<<63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantKind(t, CheckBinary(rawPlist(tc.obj, []byte{8}), DefaultLimits()))
		})
	}
}

func TestCheckRejectsHugeObjectTable(t *testing.T) {
	offsets := bytes.Repeat([]byte{8}, 1<<20+1) // one more object than MaxNodes
	offsets[0] = 10                             // top object: [1] (refSize is 3)
	wantKind(t, CheckBinary(rawPlist([]byte{0x10, 0x00, 0xA1, 0, 0, 1}, offsets), DefaultLimits()))
}

func TestCheckRejectsReferenceCycle(t *testing.T) {
	// object 0 at offset 8: an array of one reference to object 0 (self reference)
	self := rawPlist([]byte{0xA1, 0, 0, 0}, []byte{8})
	err := CheckBinary(self, DefaultLimits())
	wantKind(t, err)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("self cycle is %v, want ErrMalformed", err)
	}
	// object 0 (offset 8) -> object 1 (offset 12) -> object 0
	two := rawPlist([]byte{0xA1, 0, 0, 1, 0xA1, 0, 0, 0}, []byte{8, 12})
	err = CheckBinary(two, DefaultLimits())
	wantKind(t, err)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("two-object cycle is %v, want ErrMalformed", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := howett.Marshal(v, howett.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckRejectsTruncatedEverywhere(t *testing.T) {
	good := mustMarshal(t, map[string]any{"a": []any{1, "two", 3.5}, "b": map[string]any{"c": true}})
	if err := CheckBinary(good, DefaultLimits()); err != nil {
		t.Fatalf("control: %v", err)
	}
	for n := range len(good) {
		wantKind(t, CheckBinary(good[:n], DefaultLimits()))
	}
}

func TestCheckRejectsBadTrailer(t *testing.T) {
	base := nestedPlist(3, 1)
	tr := len(base) - 32
	if err := CheckBinary(base, DefaultLimits()); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(b []byte)
	}{
		{"offsize0", func(b []byte) { b[tr+6] = 0 }},
		{"offsize9", func(b []byte) { b[tr+6] = 9 }},
		{"refsize0", func(b []byte) { b[tr+7] = 0 }},
		{"refsize9", func(b []byte) { b[tr+7] = 9 }},
		{"top-out-of-range", func(b []byte) { binary.BigEndian.PutUint64(b[tr+16:], 4) }},
		{"table-before-magic", func(b []byte) { binary.BigEndian.PutUint64(b[tr+24:], 4) }},
		{"table-overlaps-trailer", func(b []byte) { binary.BigEndian.PutUint64(b[tr+24:], uint64(tr-1)) }},
		{"zero-objects", func(b []byte) { binary.BigEndian.PutUint64(b[tr+8:], 0) }},
		{"bad-magic", func(b []byte) { b[0] = 'x' }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Clone(base)
			tc.mut(b)
			wantKind(t, CheckBinary(b, DefaultLimits()))
		})
	}
}

func TestCheckPayloadLimit(t *testing.T) {
	l := DefaultLimits()
	// data longer than MaxPayload (header only: the length is checked before any byte is read)
	err := CheckBinary(rawPlist(bigCount(0x4, l.MaxPayload+1), []byte{8}), l)
	wantKind(t, err)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("data: %v, want ErrLimit", err)
	}
	// a UTF-16 count that is within the limit but doubles past it
	err = CheckBinary(rawPlist(bigCount(0x6, l.MaxPayload/2+1), []byte{8}), l)
	wantKind(t, err)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("utf16: %v, want ErrLimit", err)
	}
	// exactly at the limit is accepted by the payload rule
	if err := CheckBinary(rawPlist(bigCount(0x4, l.MaxPayload), []byte{8}), l); err != nil {
		t.Fatalf("at limit: %v", err)
	}
}

func TestCheckAcceptsRealPlists(t *testing.T) {
	arr := make([]any, 300)
	for i := range arr {
		arr[i] = i
	}
	doc := map[string]any{
		"int":     int64(-5),
		"big":     uint64(1 << 63),
		"real":    3.25,
		"date":    time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		"data":    bytes.Repeat([]byte{1, 2, 3}, 100),
		"uid":     howett.UID(42),
		"uni":     "héllo 世界 \U0001F600",
		"yes":     true,
		"no":      false,
		"arr":     arr,
		"nested":  map[string]any{"a": map[string]any{"b": []any{map[string]any{"c": "d"}}}},
		"longstr": string(bytes.Repeat([]byte("x"), 70000)),
	}
	if err := CheckBinary(mustMarshal(t, doc), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLimitsAreHonoured(t *testing.T) {
	bomb := nestedPlist(21, 2) // 2^22 expanded nodes
	if err := CheckBinary(bomb, DefaultLimits()); err == nil {
		t.Fatal("control: bomb accepted at defaults")
	}
	l := DefaultLimits()
	l.MaxNodes = 1 << 23
	if err := CheckBinary(bomb, l); err != nil {
		t.Fatalf("raised MaxNodes: %v", err)
	}
	small := nestedPlist(10, 2) // 2^11 nodes
	if err := CheckBinary(small, DefaultLimits()); err != nil {
		t.Fatalf("control: %v", err)
	}
	l = DefaultLimits()
	l.MaxNodes = 100
	err := CheckBinary(small, l)
	wantKind(t, err)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("lowered MaxNodes: %v, want ErrLimit", err)
	}
	l = DefaultLimits()
	l.MaxDepth = 100
	if err := CheckBinary(nestedPlist(70, 1), l); err != nil {
		t.Fatalf("raised MaxDepth: %v", err)
	}
	l.MaxDepth = 10
	if err := CheckBinary(nestedPlist(30, 1), l); !errors.Is(err, ErrLimit) {
		t.Fatalf("lowered MaxDepth: %v, want ErrLimit", err)
	}
}

func TestGuardConvertsPanicToError(t *testing.T) {
	err := guard(func() error { panic("boom") })
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
	if err := guard(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("x")
	if err := guard(func() error { return sentinel }); err != sentinel {
		t.Fatalf("err = %v", err)
	}
}
