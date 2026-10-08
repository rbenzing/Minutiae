package typedstream

import (
	"errors"
	"testing"
)

// This file holds a second reader of the same format rules, written as one linear pass with
// explicit state names. It shares no code with typedstream.go (no helper, no constant other
// than the exported caps) and is run against the implementation on generated streams and on
// the fuzz seeds.

type oState int

const (
	sMagic oState = iota // version byte, length byte, "streamtyped"
	sSysVersion
	sFindClass
	sFindPlus
	sLength
	sBody
)

// oracleResult is (text, ok) plus the class of the failure: "" none, "not", "trunc", "limit".
type oracleResult struct {
	text  string
	ok    bool
	class string
}

func oracleRead(b []byte) oracleResult {
	fail := func(c string) oracleResult { return oracleResult{class: c} }
	if len(b) > MaxInput {
		return fail("limit")
	}
	st := sMagic
	pos := 0
	scanLimit := 0
	var n uint64
	for {
		switch st {
		case sMagic:
			want := "\x04\x0bstreamtyped"
			for i := 0; i < len(want); i++ {
				if i >= len(b) {
					if i == 0 {
						return fail("not")
					}
					return fail("trunc")
				}
				if b[i] != want[i] {
					return fail("not")
				}
			}
			pos = len(want)
			st = sSysVersion
		case sSysVersion:
			_, next, c := oracleInt(b, pos)
			if c != "" {
				return fail(c)
			}
			pos = next
			scanLimit = len(b)
			if len(b)-pos > MaxScan {
				scanLimit = pos + MaxScan
			}
			st = sFindClass
		case sFindClass:
			found := -1
			for i := pos; i < scanLimit && found < 0; i++ {
				switch {
				case b[i] == 8 && oracleAt(b, i+1, "NSString"):
					found = i + 9
				case b[i] == 15 && oracleAt(b, i+1, "NSMutableString"):
					found = i + 16
				}
			}
			if found < 0 {
				if len(b)-pos >= MaxScan {
					return oracleResult{} // the window was seen in full: no string object
				}
				return fail("trunc")
			}
			pos = found
			st = sFindPlus
		case sFindPlus:
			found := -1
			for i := pos; i+1 < scanLimit && found < 0; i++ {
				if b[i] == 1 && b[i+1] == 0x2B {
					found = i + 2
				}
			}
			if found < 0 {
				if len(b) > scanLimit {
					return fail("limit")
				}
				return fail("trunc")
			}
			pos = found
			st = sLength
		case sLength:
			if pos < len(b) && b[pos] == 0x83 {
				return fail("limit")
			}
			v, next, c := oracleInt(b, pos)
			if c != "" {
				return fail(c)
			}
			n, pos = v, next
			if n > MaxText {
				return fail("limit")
			}
			st = sBody
		case sBody:
			if n > uint64(len(b)-pos) {
				return fail("trunc")
			}
			return oracleResult{text: string(b[pos : pos+int(n)]), ok: true}
		}
	}
}

func oracleAt(b []byte, i int, s string) bool {
	return i+len(s) <= len(b) && string(b[i:i+len(s)]) == s
}

// oracleInt decodes a typedstream integer at i and returns its end and a failure class.
func oracleInt(b []byte, i int) (uint64, int, string) {
	if i >= len(b) {
		return 0, 0, "trunc"
	}
	tag := b[i]
	if tag < 0x80 {
		return uint64(tag), i + 1, ""
	}
	width := map[byte]int{0x81: 2, 0x82: 4, 0x83: 8}[tag]
	if width == 0 {
		return 0, 0, "not"
	}
	if i+1+width > len(b) {
		return 0, 0, "trunc"
	}
	var v uint64
	for k := 0; k < width; k++ {
		v += uint64(b[i+1+k]) << (8 * k)
	}
	return v, i + 1 + width, ""
}

func classOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotTypedstream):
		return "not"
	case errors.Is(err, ErrTruncated):
		return "trunc"
	case errors.Is(err, ErrLimit):
		return "limit"
	}
	return "other:" + err.Error()
}

func compareToOracle(t *testing.T, name string, b []byte) {
	t.Helper()
	text, ok, err := ExtractText(b, view(64<<20))
	want := oracleRead(b)
	if text != want.text || ok != want.ok || classOf(err) != want.class {
		t.Fatalf("%s: implementation (%.40q, %v, %q), oracle (%.40q, %v, %q)",
			name, text, ok, classOf(err), want.text, want.ok, want.class)
	}
}

func TestExtractTextMatchesIndependentReader(t *testing.T) {
	rng := &xorshift{state: 0x9E3779B97F4A7C15}
	lengths := []int{0, 1, 2, 126, 127, 128, 129, 255, 256, 257, 4096, 65535, 65536, 65537}
	for i := 0; i < 500; i++ {
		n := lengths[rng.Intn(len(lengths))]
		if rng.Intn(3) == 0 {
			n = rng.Intn(300)
		}
		raw := make([]byte, n)
		for k := range raw {
			raw[k] = byte(rng.Intn(256))
		}
		var opts []buildOpt
		if rng.Intn(2) == 0 {
			opts = append(opts, mutable())
		}
		if rng.Intn(5) == 0 {
			opts = append(opts, padding(rng.Intn(MaxScan+300)))
		}
		if rng.Intn(8) == 0 {
			opts = append(opts, withoutString())
		}
		st := buildStream(string(raw), opts...)
		if rng.Intn(4) == 0 {
			st = st[:rng.Intn(len(st)+1)]
		}
		compareToOracle(t, "generated", st)
	}
	for _, s := range fuzzSeeds() {
		compareToOracle(t, "seed", s)
		if len(s) > 0 {
			m := append([]byte(nil), s...)
			m[rng.Intn(len(m))] ^= byte(1 << uint(rng.Intn(8)))
			compareToOracle(t, "mutated seed", m)
		}
	}
}

// xorshift is a tiny deterministic generator, so the generated streams are the same on every
// run and Go version.
type xorshift struct{ state uint64 }

func (x *xorshift) Intn(n int) int {
	x.state ^= x.state << 13
	x.state ^= x.state >> 7
	x.state ^= x.state << 17
	return int(x.state % uint64(n))
}
