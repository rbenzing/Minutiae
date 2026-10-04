package evidence

import (
	"errors"
	"fmt"
	"strings"
)

// ErrReadSQLRefused is returned by a ReadHandle query that is not one plain read
// statement. Nothing is executed.
var ErrReadSQLRefused = errors.New("ReadHandle accepts one SELECT or WITH statement only")

// refusedReadKeywords may not appear (outside string literals, quoted
// identifiers and comments) in a statement a ReadHandle runs. A statement that
// starts with SELECT or WITH and holds no second statement cannot run most of
// these anyway; the list also stops a WITH ... UPDATE and keeps the rule
// obvious. END is not listed: it closes a CASE expression, and a transaction END
// needs a statement of its own, which the single-statement rule refuses.
var refusedReadKeywords = map[string]bool{
	"pragma": true, "attach": true, "detach": true, "commit": true, "begin": true,
	"rollback": true, "savepoint": true, "release": true, "vacuum": true,
	"insert": true, "update": true, "delete": true, "create": true, "drop": true,
	"alter": true, "reindex": true, "analyze": true,
	// replace is also a function: only REPLACE INTO is refused (see checkReadSQL)
}

// checkReadSQL accepts exactly one statement that starts with SELECT or WITH
// (after whitespace and comments), has at most one trailing ';', and mentions
// none of refusedReadKeywords outside literals. It is the first layer that keeps
// a read transaction read-only; ReadTx's detect-and-roll-back is the second.
func checkReadSQL(q string) error {
	i := skipBlanks(q, 0)
	w, _ := wordAt(q, i)
	if lw := strings.ToLower(w); lw != "select" && lw != "with" {
		return fmt.Errorf("%w: a read statement starts with SELECT or WITH", ErrReadSQLRefused)
	}
	for i < len(q) {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && strings.HasPrefix(q[i:], "--"), c == '/' && strings.HasPrefix(q[i:], "/*"):
			i = skipBlanks(q, i)
		case c == '\'' || c == '"' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := closeQuote(q, i, end)
			if j < 0 {
				return fmt.Errorf("%w: unterminated quoted text", ErrReadSQLRefused)
			}
			i = j
		case c == ';':
			if rest := skipBlanks(q, i+1); rest != len(q) {
				return fmt.Errorf("%w: a second statement follows the first", ErrReadSQLRefused)
			}
			return nil
		case isWordStart(c):
			w, next := wordAt(q, i)
			lw := strings.ToLower(w)
			if refusedReadKeywords[lw] {
				return fmt.Errorf("%w: %s is not allowed", ErrReadSQLRefused, strings.ToUpper(lw))
			}
			if lw == "replace" && skipBlanks(q, next) < len(q) && q[skipBlanks(q, next)] != '(' {
				return fmt.Errorf("%w: REPLACE is not allowed", ErrReadSQLRefused)
			}
			i = next
		case c >= '0' && c <= '9': // a number, with its suffix letters (1e5, 0x1F)
			for i < len(q) && (isWordStart(q[i]) || q[i] >= '0' && q[i] <= '9' || q[i] == '.') {
				i++
			}
		default:
			i++
		}
	}
	return nil
}

func isWordStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// wordAt returns the identifier-like word that starts at i and the index after it.
func wordAt(q string, i int) (string, int) {
	j := i
	for j < len(q) && (isWordStart(q[j]) || q[j] >= '0' && q[j] <= '9' || q[j] == '$') {
		j++
	}
	return q[i:j], j
}

// skipBlanks returns the index of the first byte at or after i that is not
// whitespace or part of a comment. An unterminated block comment runs to the end.
func skipBlanks(q string, i int) int {
	for i < len(q) {
		switch {
		case q[i] == ' ' || q[i] == '\t' || q[i] == '\n' || q[i] == '\r' || q[i] == '\f' || q[i] == '\v':
			i++
		case strings.HasPrefix(q[i:], "--"):
			if j := strings.IndexByte(q[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(q)
			}
		case strings.HasPrefix(q[i:], "/*"):
			if j := strings.Index(q[i+2:], "*/"); j >= 0 {
				i += 2 + j + 2
			} else {
				i = len(q)
			}
		default:
			return i
		}
	}
	return i
}

// closeQuote returns the index after the quote that closes the one opened at i
// (a doubled closing quote is an escaped one, except for [ ]), or -1.
func closeQuote(q string, i int, end byte) int {
	for j := i + 1; j < len(q); j++ {
		if q[j] != end {
			continue
		}
		if end != ']' && j+1 < len(q) && q[j+1] == end {
			j++
			continue
		}
		return j + 1
	}
	return -1
}
