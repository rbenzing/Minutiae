package sqlitefile

import (
	"bytes"
	"math"
	"strconv"
	"unicode/utf8"
)

// ConvStatus says whether ConvertForComparison could decide the engine's conversion.
type ConvStatus uint8

// The conversion statuses.
const (
	ConvOK        ConvStatus = iota // the returned value is what the engine compares (v itself when nothing converts)
	ConvUndecided                   // the engine converts, but not to a value this reader reproduces exactly
)

// ConvertForComparison applies the engine's affinity rules to the value v of one
// operand of an equality, whose own affinity is own (AffBlob for a constant or a
// column with no affinity), when the OTHER operand has affinity other:
//   - other is INTEGER, REAL or NUMERIC and own is TEXT or none: NUMERIC
//     affinity is applied to v. Text that is, in whole (blanks around allowed),
//     a number this reader reproduces exactly becomes an integer (an integral
//     value that fits) or a real; other text, blobs and NULL stay as they are.
//   - other is TEXT and own is none: TEXT affinity is applied to v. An integer
//     becomes its decimal text (UTF-8). A real is ConvUndecided (its spelling is
//     the engine's).
//   - anything else (equal classes, own stronger than other): v is returned
//     unchanged.
//
// A value that is Omitted, Unread or Clipped, or text that is not valid in its
// encoding, is ConvUndecided whenever a conversion would apply, and unchanged
// otherwise. On ConvUndecided the returned value is v itself.
//
// Nothing here guesses: a conversion is made only when this reader reproduces
// the engine's result exactly (an integer that fits int64, a decimal of at most
// 15 significant digits whose decimal exponent shift is at most 22, an integer
// rendered as decimal text). The exactness is proven against the reference
// engine build; see docs/limitations.md.
func ConvertForComparison(v Value, own, other Affinity) (Value, ConvStatus) {
	numeric := isNumericAffinity(other) && (own == AffText || own == AffBlob)
	toText := other == AffText && own == AffBlob
	if !numeric && !toText {
		return v, ConvOK
	}
	if v.Omitted || v.Unread || v.Clipped {
		return v, ConvUndecided
	}
	switch v.Kind {
	case KindText:
		if !numeric {
			return v, ConvOK
		}
		var b []byte
		switch v.Enc {
		case EncUTF16LE, EncUTF16BE:
			s, ok := v.Text()
			if !ok {
				return v, ConvUndecided
			}
			b = []byte(s)
		default:
			if !utf8.Valid(v.Bytes) {
				return v, ConvUndecided
			}
			b = v.Bytes
		}
		switch n, class := numericOfText(b); class {
		case textConverted:
			return n, ConvOK
		case textUndecided:
			return v, ConvUndecided
		}
		return v, ConvOK
	case KindInt:
		if toText {
			s := strconv.AppendInt(nil, v.Int, 10)
			return Value{Kind: KindText, Bytes: s, Len: int64(len(s)), Enc: EncUTF8}, ConvOK
		}
	case KindFloat:
		if toText && !math.IsNaN(v.Float) { // a NaN is NULL to the engine and equals nothing
			return v, ConvUndecided // the engine's spelling of a real is not reproduced
		}
	}
	return v, ConvOK
}

func isNumericAffinity(a Affinity) bool { return a == AffInteger || a == AffReal || a == AffNumeric }

// textClass is the verdict of numericOfText.
type textClass uint8

const (
	textPlain     textClass = iota // provably not a number: it stays text
	textConverted                  // an exact number: the returned Value
	textUndecided                  // number-like, but not reproduced exactly
)

const (
	maxNumberText  = 64 // bytes of number-like text (after the blanks) the classifier reads
	maxSigDigits   = 15 // significant digits of a decimal that is a correctly rounded float64
	maxDecimalExp  = 22 // |decimal exponent shift| for which 10^shift is exact in a float64
	maxExpDigitsIn = 4  // significant digits of an explicit exponent that are read
)

// isBlank is the engine's isspace: space, tab, newline, vertical tab, form feed
// and carriage return.
func isBlank(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// numericOfText classifies text (already known to be valid) as the engine's
// NUMERIC affinity sees it: the whole text, blanks around it allowed, is a
// number or it is text. It reads at most maxNumberText bytes after the blanks
// and allocates nothing in proportion to the input. A number that fits int64
// and is written as an integer is an integer; a decimal of at most 15
// significant digits and an exponent shift of at most 22 is a real, or the
// integer of the same value when integral and in range (the engine's rule); any
// other number-like text is undecided. A NUL ends the text, as the engine reads
// it (the text "7" then NUL then "abc" is the integer 7); the spellings of infinity and NaN are text.
func numericOfText(b []byte) (Value, textClass) {
	if k := bytes.IndexByte(b, 0); k >= 0 {
		b = b[:k]
	}
	i, j := 0, len(b)
	for i < j && isBlank(b[i]) {
		i++
	}
	for j > i && isBlank(b[j-1]) {
		j--
	}
	if i == j {
		return Value{}, textPlain
	}
	body := b[i:j]
	longer := len(body) > maxNumberText
	n := min(len(body), maxNumberText)
	p, neg := 0, false
	if body[0] == '+' || body[0] == '-' {
		neg = body[0] == '-'
		p++
	}
	mant := p
	for p < n && isDigit(body[p]) {
		p++
	}
	intDigits := p - mant
	sawDot, fracDigits := false, 0
	if p < n && body[p] == '.' {
		sawDot = true
		p++
		f0 := p
		for p < n && isDigit(body[p]) {
			p++
		}
		fracDigits = p - f0
	}
	if intDigits+fracDigits == 0 {
		return Value{}, textPlain
	}
	sawExp, expNeg := false, false
	var exp int
	if p < n && (body[p] == 'e' || body[p] == 'E') {
		sawExp = true
		p++
		if p < n && (body[p] == '+' || body[p] == '-') {
			expNeg = body[p] == '-'
			p++
		}
		e0 := p
		for p < n && isDigit(body[p]) {
			p++
		}
		if p == e0 {
			return Value{}, textPlain
		}
		sig := e0
		for sig < p && body[sig] == '0' {
			sig++
		}
		if p-sig > maxExpDigitsIn {
			exp = 1 << 20 // far beyond the guard: undecided below
		} else {
			for k := sig; k < p; k++ {
				exp = exp*10 + int(body[k]-'0')
			}
		}
		if expNeg {
			exp = -exp
		}
	}
	if p != n {
		return Value{}, textPlain // a byte that cannot continue a number
	}
	if longer {
		return Value{}, textUndecided
	}
	if !sawDot && !sawExp {
		var mag uint64
		for k := mant; k < n; k++ {
			d := uint64(body[k] - '0')
			if mag > (math.MaxUint64-d)/10 {
				return Value{}, textUndecided
			}
			mag = mag*10 + d
		}
		switch {
		case neg && mag <= 1<<63:
			return Value{Kind: KindInt, Int: int64(-mag)}, textConverted
		case !neg && mag <= math.MaxInt64:
			return Value{Kind: KindInt, Int: int64(mag)}, textConverted
		}
		return Value{}, textUndecided
	}
	sig := 0 // significant digits: from the first non-zero digit of the mantissa
	started := false
	for k := mant; k < n && (isDigit(body[k]) || body[k] == '.'); k++ {
		if body[k] == '.' {
			continue
		}
		if body[k] != '0' {
			started = true
		}
		if started {
			sig++
		}
	}
	shift := exp - fracDigits
	if sig > maxSigDigits || shift > maxDecimalExp || shift < -maxDecimalExp {
		return Value{}, textUndecided
	}
	f, err := strconv.ParseFloat(string(body[:n]), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return Value{}, textUndecided
	}
	if f == math.Trunc(f) && f > -(1<<63) && f < 1<<63 {
		return Value{Kind: KindInt, Int: int64(f)}, textConverted
	}
	return Value{Kind: KindFloat, Float: f}, textConverted
}
