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
	// ConvOK: the returned value is what the engine compares (v itself when
	// nothing converts).
	ConvOK ConvStatus = iota
	// ConvUndecided: the engine converts, but not to a value this reader
	// reproduces exactly.
	ConvUndecided
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
		var n Value
		var class textClass
		switch v.Enc {
		case EncUTF16LE, EncUTF16BE:
			// Only a bounded prefix of the decoded text matters to the classifier,
			// so the text is validated in one pass and never decoded whole.
			var buf [maxNumberText + 1]byte
			k, ok := utf16Prefix(v.Bytes, v.Enc == EncUTF16BE, &buf)
			if !ok {
				return v, ConvUndecided
			}
			n, class = numericOfText(buf[:k])
		default:
			if !utf8.Valid(v.Bytes) {
				return v, ConvUndecided
			}
			n, class = numericOfText(v.Bytes)
		}
		switch class {
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

// numericOfText classifies UTF-8 text (already known to be valid) as the
// engine's NUMERIC affinity sees it: the whole text, blanks around it allowed,
// is a number or it is text. Besides skipping blanks and looking for a NUL (a
// scan, no allocation) it examines at most maxNumberText bytes after the
// blanks, allocates only the few bytes of a decimal's spelling whatever the
// length of b, and never copies b. UTF-16 text is not passed whole: utf16Prefix
// builds the bounded UTF-8 prefix that gives the same outcome, so no encoding is
// decoded in full.
//
// A number that fits int64 and is written as an integer is an integer; a
// decimal of at most 15 significant digits and an exponent shift of at most 22
// is a real, or the integer of the same value when integral and in range (the
// engine's rule); any other number-like text is undecided. A NUL ends the text,
// as the engine reads it (the text "7", NUL, "abc" is the integer 7). The
// spellings of infinity and NaN are text.
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

// utf16Prefix validates UTF-16 text in one pass (an odd byte count, a high
// surrogate not followed by a low one and a lone low surrogate are rejected,
// as Value.Text does) and writes into buf exactly what numericOfText needs of
// the decoded text, as UTF-8: the text after the leading blanks and before the
// first NUL, cut at maxNumberText bytes, followed by one extra non-blank byte
// when more non-blank text follows the cut (so the classifier sees a body over
// the limit, as it does for UTF-8 text). Classifying that prefix gives the
// same outcome as classifying the whole decoded text. It allocates nothing.
func utf16Prefix(b []byte, bigEndian bool, buf *[maxNumberText + 1]byte) (int, bool) {
	if len(b)%2 != 0 {
		return 0, false
	}
	unit := func(i int) rune {
		if bigEndian {
			return rune(b[i])<<8 | rune(b[i+1])
		}
		return rune(b[i+1])<<8 | rune(b[i])
	}
	n := 0
	started, ended, over := false, false, false
	for i := 0; i < len(b); i += 2 {
		r := unit(i)
		switch {
		case r >= 0xd800 && r < 0xdc00: // high surrogate: a low one must follow
			if i+2 >= len(b) {
				return 0, false
			}
			lo := unit(i + 2)
			if lo < 0xdc00 || lo >= 0xe000 {
				return 0, false
			}
			r = 0x10000 + (r-0xd800)<<10 + (lo - 0xdc00)
			i += 2
		case r >= 0xdc00 && r < 0xe000: // lone low surrogate
			return 0, false
		}
		if ended { // validation goes on to the end, collection does not
			continue
		}
		if r == 0 {
			ended = true
			continue
		}
		if !started && r < 0x80 && isBlank(byte(r)) {
			continue
		}
		started = true
		var e [utf8.UTFMax]byte
		for _, c := range e[:utf8.EncodeRune(e[:], r)] {
			if n < maxNumberText {
				buf[n] = c
				n++
			} else if !isBlank(c) {
				over = true
			}
		}
	}
	if over {
		buf[n] = 'x'
		n++
	}
	return n, true
}
