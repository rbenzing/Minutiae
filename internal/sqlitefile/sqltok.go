package sqlitefile

// A tolerant SQL tokenizer for the CREATE statements of the schema table. It
// reads bare words, "x", `x` and [x] identifiers, 'x' strings, numbers,
// punctuation, -- and /* */ comments. It works on any bytes, never panics,
// and produces at most one token per input byte plus the final end token, so
// the work of a parse is linear in the input.

type tokKind uint8

const (
	tkEOF    tokKind = iota
	tkWord           // a bare word: identifier or keyword
	tkQuoted         // a quoted identifier: "x", `x`, [x]
	tkString         // a string literal: 'x'
	tkNumber         // a numeric literal (its text is not validated)
	tkPunct          // one punctuation byte
	tkBad            // an unterminated quote, or a byte no token can start with
)

type token struct {
	kind       tokKind
	text       string // tkWord: as written; tkQuoted, tkString: unquoted; tkNumber, tkPunct: as written
	start, end int    // byte range in the source
}

// isPunct reports whether t is the punctuation byte c.
func (t token) isPunct(c byte) bool { return t.kind == tkPunct && t.text[0] == c }

// isWord reports whether t is the bare word w (ASCII case-insensitive; w is
// given in upper case). A quoted identifier is never a keyword.
func (t token) isWord(w string) bool { return t.kind == tkWord && asciiEqualFold(t.text, w) }

type lexer struct {
	s     string
	i     int
	steps int // tokens produced
}

func isWordStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordByte(c byte) bool { return isWordStart(c) || (c >= '0' && c <= '9') || c == '$' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// next returns the next token. At the end of the input it returns tkEOF
// forever.
func (l *lexer) next() token {
	s := l.s
	for {
		for l.i < len(s) && (s[l.i] == ' ' || s[l.i] == '\t' || s[l.i] == '\n' || s[l.i] == '\r' || s[l.i] == '\f') {
			l.i++
		}
		if l.i+1 < len(s) && s[l.i] == '-' && s[l.i+1] == '-' {
			for l.i < len(s) && s[l.i] != '\n' {
				l.i++
			}
			continue
		}
		if l.i+1 < len(s) && s[l.i] == '/' && s[l.i+1] == '*' {
			l.i += 2
			for l.i < len(s) && (s[l.i] != '*' || l.i+1 >= len(s) || s[l.i+1] != '/') {
				l.i++
			}
			l.i = min(l.i+2, len(s)) // an unterminated comment runs to the end, as in the engine
			continue
		}
		break
	}
	if l.i >= len(s) {
		return token{kind: tkEOF, start: len(s), end: len(s)}
	}
	l.steps++
	start := l.i
	c := s[start]
	switch {
	case isWordStart(c):
		l.i++
		for l.i < len(s) && isWordByte(s[l.i]) {
			l.i++
		}
		return token{kind: tkWord, text: s[start:l.i], start: start, end: l.i}
	case isDigit(c) || (c == '.' && start+1 < len(s) && isDigit(s[start+1])):
		l.i++
		for l.i < len(s) {
			d := s[l.i]
			if isWordByte(d) || d == '.' {
				l.i++
				continue
			}
			// An exponent sign belongs to the number: 1e+5, 2.5E-3 (not for hex).
			if (d == '+' || d == '-') && (s[l.i-1] == 'e' || s[l.i-1] == 'E') && !isHexNumber(s[start:l.i]) {
				l.i++
				continue
			}
			break
		}
		return token{kind: tkNumber, text: s[start:l.i], start: start, end: l.i}
	case c == '\'' || c == '"' || c == '`':
		text, end, ok := unquote(s, start, c)
		l.i = end
		if !ok {
			return token{kind: tkBad, start: start, end: end}
		}
		kind := tkQuoted
		if c == '\'' {
			kind = tkString
		}
		return token{kind: kind, text: text, start: start, end: end}
	case c == '[':
		j := start + 1
		for j < len(s) && s[j] != ']' {
			j++
		}
		if j >= len(s) {
			l.i = len(s)
			return token{kind: tkBad, start: start, end: len(s)}
		}
		l.i = j + 1
		return token{kind: tkQuoted, text: s[start+1 : j], start: start, end: l.i}
	case c == 0 || c < 0x20 || c == 0x7f:
		l.i++
		return token{kind: tkBad, start: start, end: l.i}
	}
	l.i++
	return token{kind: tkPunct, text: s[start:l.i], start: start, end: l.i}
}

func isHexNumber(s string) bool {
	return len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
}

// unquote reads the quoted text starting at s[start] (the quote q), where a
// doubled quote stands for one. It returns the text, the index after the
// closing quote, and false when the closing quote is missing (end is then
// len(s)).
func unquote(s string, start int, q byte) (text string, end int, ok bool) {
	i := start + 1
	escaped := false
	for i < len(s) {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				escaped = true
				i += 2
				continue
			}
			text = s[start+1 : i]
			if escaped {
				b := make([]byte, 0, len(text))
				for j := 0; j < len(text); j++ {
					b = append(b, text[j])
					if text[j] == q {
						j++ // the second quote of a pair
					}
				}
				text = string(b)
			}
			return text, i + 1, true
		}
		i++
	}
	return "", len(s), false
}
