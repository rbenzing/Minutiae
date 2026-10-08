package records

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// ErrInvalidQuery is the root of every refusal of a search query (grammar, limits, a term with
// nothing to search for). The message never echoes the input raw: what it quotes is escaped to ASCII
// and cut short.
var ErrInvalidQuery = errors.New("invalid search query")

// Query limits.
const (
	// MaxQueryBytes is the size of the whole query text.
	MaxQueryBytes = 4096
	// MaxQueryTerms is the number of items (words, phrases, prefixes, negated or not) of one query.
	MaxQueryTerms = 64
	// MaxTermBytes is the size of one word or phrase (of the whole input for a substring search).
	MaxTermBytes = 256
	// MinPrefixChars is the number of letters and digits a prefix needs before its star.
	MinPrefixChars = 2
	// MinSubstringChars is the number of characters of a substring search (the trigram index holds
	// nothing shorter).
	MinSubstringChars = 3
	// MaxTerms is the number of terms CompileTerms accepts.
	MaxTerms = 1000
)

// Column selects the full-text column a query is limited to.
type Column int

// The columns.
const (
	ColAny Column = iota
	ColSummary
	ColBody
)

// TextOptions are the options of CompileQuery.
type TextOptions struct {
	// Substring searches the whole input as one literal substring (the trigram index): no grammar,
	// quotes and stars are text.
	Substring bool
	// Column limits the search to one column.
	Column Column
	// Rank asks for relevance order. It is refused with Substring.
	Rank bool
}

// NeedleKind says how a positive term of a query matches, for the snippet that shows it.
type NeedleKind string

// The needle kinds.
const (
	NeedleWord      NeedleKind = "word"
	NeedlePrefix    NeedleKind = "prefix"
	NeedlePhrase    NeedleKind = "phrase"
	NeedleSubstring NeedleKind = "substring"
)

// Needle is a positive term of a compiled query, as normalized text.
type Needle struct {
	Kind NeedleKind
	Text string
}

// Term is one literal term of CompileTerms (a keyword list entry): Text is never grammar.
type Term struct {
	Text      string
	Substring bool
}

// TextQuery is a compiled search query: the full-text table, the FTS5 expression to bind as the one
// MATCH parameter, and what a snippet should highlight. It is immutable.
type TextQuery struct {
	table   string
	match   string
	column  Column
	rank    bool
	needles []Needle
}

// Table is the full-text table the query runs on: evidence.FTSWordTable or evidence.FTSSubTable.
func (q *TextQuery) Table() string { return q.table }

// Match is the FTS5 expression, to be bound as a parameter, never spliced into SQL.
func (q *TextQuery) Match() string { return q.match }

// Rank reports whether the caller asked for relevance order.
func (q *TextQuery) Rank() bool { return q.rank }

// Needles returns the positive terms of the query (negated ones are not highlighted), normalized, in
// query order. The slice is a copy.
func (q *TextQuery) Needles() []Needle { return append([]Needle(nil), q.needles...) }

// canonical is the form a cursor fingerprints: the table, column and rank together with the
// expression, so a cursor made for one query is refused for another.
func (q *TextQuery) canonical() string {
	return q.table + "|" + columnName(q.column) + "|" + strconv.FormatBool(q.rank) + "|" + q.match
}

func columnName(c Column) string {
	switch c {
	case ColSummary:
		return "summary"
	case ColBody:
		return "body"
	default:
		return "any"
	}
}

func invalidQuery(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidQuery, fmt.Sprintf(format, args...))
}

// echo quotes part of the input for an error message: ASCII-escaped (so a control character, a bidi
// override or an escape sequence never reaches a terminal) and cut to 32 characters.
func echo(s string) string {
	const (
		limit      = 32  // characters
		maxEscaped = 120 // bytes of the escaped quote, so one error stays under 300 bytes
	)
	var b strings.Builder
	n, cut := 0, false
	for i, r := range s {
		esc := strconv.QuoteToASCII(string(r))
		esc = esc[1 : len(esc)-1]
		if n >= limit || b.Len()+len(esc) > maxEscaped {
			cut = i < len(s)
			break
		}
		b.WriteString(esc)
		n++
	}
	q := `"` + b.String() + `"`
	if cut {
		q += "..."
	}
	return q
}

// checkInput refuses text that cannot be searched for at all: not UTF-8, a NUL.
func checkInput(s string) error {
	if !utf8.ValidString(s) {
		return invalidQuery("the text is not valid UTF-8")
	}
	if strings.IndexByte(s, 0) >= 0 {
		return invalidQuery("the text holds a NUL character")
	}
	return nil
}

// quoteFTS makes s one FTS5 string: it is wrapped in quotes and every quote inside is doubled. s is
// normalized text, so it holds no NUL and no control character.
func quoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// lastToken is the last run of letters and digits of s (what a prefix star attaches to), folded the
// way the index folds, or "" when s holds none.
func lastToken(s string) string {
	f := evidence.FTSFoldDiacritics(s)
	end := strings.LastIndexFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) })
	if end < 0 {
		return ""
	}
	_, size := utf8.DecodeRuneInString(f[end:])
	f = f[:end+size]
	start := strings.LastIndexFunc(f, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	if start < 0 {
		return f
	}
	_, size = utf8.DecodeRuneInString(f[start:])
	return f[start+size:]
}

// countWordChars counts the letters and digits of s.
func countWordChars(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			n++
		}
	}
	return n
}

// normalizeTerm normalizes text for the index with the one function the index text goes through and
// requires that something searchable is left: at least one letter or digit.
func normalizeTerm(text string) (string, error) {
	if len(text) > MaxTermBytes {
		return "", invalidQuery("a term is longer than %d bytes", MaxTermBytes)
	}
	n := evidence.NormalizeText(text)
	switch {
	case n == "":
		return "", invalidQuery("%s has nothing to search for (it is blank, control or zero-width characters)", echo(text))
	case countWordChars(n) == 0:
		return "", invalidQuery("%s has no letter or digit to search for", echo(text))
	}
	return n, nil
}

// substringTerm normalizes a substring needle and checks its length.
func substringTerm(text string) (string, error) {
	n, err := normalizeTerm(text)
	if err != nil {
		return "", err
	}
	// count what the trigram tokenizer keeps: the marks it strips are not characters of the index
	if utf8.RuneCountInString(evidence.FTSFoldDiacritics(n)) < MinSubstringChars {
		return "", invalidQuery("%s is shorter than %d characters, the least a substring search can find", echo(text), MinSubstringChars)
	}
	return n, nil
}

// withColumn limits expr to one column with the one column filter the compiler ever emits.
func withColumn(expr string, c Column) string {
	if c == ColAny {
		return expr
	}
	return columnName(c) + " : (" + expr + ")"
}

// CompileQuery compiles the closed search grammar into an FTS5 expression:
//
//	query  := group ( "OR" group )*            group := item ( ["AND"] item )*
//	item   := ["-"] ( word | phrase | prefix ) word := run of non-space, non-quote characters
//	phrase := '"' text '"'  (a closing quote is followed by space or the end)
//	prefix := word "*"       (only a trailing * of an unquoted word)
//
// OR and AND are keywords only in upper case. A leading "-" negates an item; a hyphen inside a word
// is part of it. Every group needs a positive item; its negations apply to that group. Every word and
// phrase goes through evidence.NormalizeText and is emitted as one FTS5 string with its quotes
// doubled, so nothing the input holds (a star, a caret, a colon, parentheses, NEAR, AND, OR, NOT)
// can act as FTS5 syntax: the only operators in the expression are the compiler's own. The expression
// is built from the pieces `(`, `)`, ` AND `, ` OR `, ` NOT `, `"text"`, `"text" *`, and the one
// leading `summary : (` or `body : (` when a column is asked for.
//
// With Substring the whole input is no grammar: it is one normalized needle, at least
// MinSubstringChars long, matched on the trigram table.
func CompileQuery(input string, o TextOptions) (*TextQuery, error) {
	if o.Column < ColAny || o.Column > ColBody {
		return nil, invalidQuery("unknown column %d", int(o.Column))
	}
	if o.Substring && o.Rank {
		return nil, invalidQuery("relevance ranking is not available for a substring search")
	}
	if len(input) > MaxQueryBytes {
		return nil, invalidQuery("the query is longer than %d bytes", MaxQueryBytes)
	}
	if o.Substring && len(input) > MaxTermBytes {
		return nil, invalidQuery("a term is longer than %d bytes", MaxTermBytes)
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	q := &TextQuery{column: o.Column, rank: o.Rank}
	if o.Substring {
		n, err := substringTerm(input)
		if err != nil {
			return nil, err
		}
		q.table, q.match = evidence.FTSSubTable, withColumn(quoteFTS(n), o.Column)
		q.needles = []Needle{{Kind: NeedleSubstring, Text: n}}
	} else {
		expr, needles, err := compileGrammar(input)
		if err != nil {
			return nil, err
		}
		q.table, q.match, q.needles = evidence.FTSWordTable, withColumn(expr, o.Column), needles
	}
	return q, nil
}

// item is one parsed word, phrase or prefix: the normalized text, ready for the expression.
type item struct {
	kind NeedleKind
	text string
	neg  bool
}

func (i item) expr() string {
	if i.kind == NeedlePrefix {
		return quoteFTS(i.text) + " *"
	}
	return quoteFTS(i.text)
}

// group is the items between two ORs.
type group struct{ pos, neg []item }

func (g group) expr() string {
	parts := make([]string, len(g.pos))
	for i, it := range g.pos {
		parts[i] = it.expr()
	}
	s := "(" + strings.Join(parts, " AND ") + ")"
	if len(g.neg) > 0 {
		parts = parts[:0]
		for _, it := range g.neg {
			parts = append(parts, it.expr())
		}
		s += " NOT (" + strings.Join(parts, " OR ") + ")"
	}
	return s
}

// isSeparator reports whether r separates words in a query: whitespace and the characters the
// normalization maps to a blank.
func isSeparator(r rune) bool {
	return unicode.IsSpace(r) || unicode.In(r, unicode.Cc, unicode.Zs, unicode.Zl, unicode.Zp)
}

// compileGrammar parses the closed grammar and returns the expression and the positive needles.
func compileGrammar(input string) (string, []Needle, error) {
	var (
		groups     []group
		cur        group
		items      int
		needItem   bool // the last token was AND or OR: an item must follow
		sawContent bool
		needles    []Needle
	)
	add := func(it item) error {
		if items++; items > MaxQueryTerms {
			return invalidQuery("the query has more than %d terms", MaxQueryTerms)
		}
		if it.neg {
			cur.neg = append(cur.neg, it)
		} else {
			cur.pos = append(cur.pos, it)
			needles = append(needles, Needle{Kind: it.kind, Text: it.text})
		}
		needItem, sawContent = false, true
		return nil
	}
	s := input
	for {
		s = strings.TrimLeftFunc(s, isSeparator)
		if s == "" {
			break
		}
		neg := false
		if s[0] == '-' {
			neg, s = true, s[1:]
			if r, _ := utf8.DecodeRuneInString(s); s == "" || isSeparator(r) {
				return "", nil, invalidQuery("a minus must be followed by the term it negates")
			}
		}
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				return "", nil, invalidQuery("a quote is not closed")
			}
			text := s[1 : 1+end]
			s = s[2+end:]
			if r, _ := utf8.DecodeRuneInString(s); s != "" && !isSeparator(r) {
				return "", nil, invalidQuery("a closing quote must be followed by a blank or the end of the query")
			}
			n, err := normalizeTerm(text)
			if err != nil {
				return "", nil, err
			}
			if err := add(item{kind: NeedlePhrase, text: n, neg: neg}); err != nil {
				return "", nil, err
			}
			continue
		}
		end := strings.IndexFunc(s, func(r rune) bool { return r == '"' || isSeparator(r) })
		if end < 0 {
			end = len(s)
		}
		word := s[:end]
		s = s[end:]
		if s != "" && s[0] == '"' {
			return "", nil, invalidQuery("a quote inside the word %s (put a phrase between blanks)", echo(word))
		}
		if !neg && (word == "OR" || word == "AND") {
			switch {
			case needItem || len(cur.pos)+len(cur.neg) == 0:
				return "", nil, invalidQuery("%s needs a term before it", word)
			case word == "AND":
				needItem = true
			default:
				if len(cur.pos) == 0 {
					return "", nil, invalidQuery("a group before OR has no positive term")
				}
				groups, cur, needItem = append(groups, cur), group{}, true
			}
			continue
		}
		kind := NeedleWord
		if strings.HasSuffix(word, "*") {
			kind, word = NeedlePrefix, word[:len(word)-1]
		}
		n, err := normalizeTerm(word)
		if err != nil {
			return "", nil, err
		}
		if kind == NeedlePrefix && countWordChars(lastToken(n)) < MinPrefixChars {
			return "", nil, invalidQuery("a prefix needs at least %d letters or digits before its star", MinPrefixChars)
		}
		if err := add(item{kind: kind, text: n, neg: neg}); err != nil {
			return "", nil, err
		}
	}
	switch {
	case !sawContent:
		return "", nil, invalidQuery("the query is empty")
	case needItem:
		return "", nil, invalidQuery("the query ends with AND or OR")
	case len(cur.pos) == 0:
		return "", nil, invalidQuery("a group has no positive term (only negations)")
	}
	groups = append(groups, cur)
	exprs := make([]string, len(groups))
	for i, g := range groups {
		exprs[i] = g.expr()
	}
	return strings.Join(exprs, " OR "), needles, nil
}

// CompileTerms compiles a keyword list: one query per term, each term literal text (no grammar: an
// OR, a minus or a quote in it is text), as one phrase on the word table or, with Substring, one
// substring on the trigram table. At least one and at most MaxTerms terms are accepted, each at most
// MaxTermBytes with something searchable in it; the first term that is refused refuses the list.
func CompileTerms(terms []Term) ([]*TextQuery, error) {
	if len(terms) == 0 {
		return nil, invalidQuery("there are no terms")
	}
	if len(terms) > MaxTerms {
		return nil, invalidQuery("there are more than %d terms", MaxTerms)
	}
	out := make([]*TextQuery, len(terms))
	for i, t := range terms {
		if len(t.Text) > MaxTermBytes {
			return nil, fmt.Errorf("term %d: %w", i+1, invalidQuery("a term is longer than %d bytes", MaxTermBytes))
		}
		if err := checkInput(t.Text); err != nil {
			return nil, fmt.Errorf("term %d: %w", i+1, err)
		}
		q := &TextQuery{}
		var err error
		var n string
		if t.Substring {
			if n, err = substringTerm(t.Text); err == nil {
				q.table, q.needles = evidence.FTSSubTable, []Needle{{Kind: NeedleSubstring, Text: n}}
			}
		} else if n, err = normalizeTerm(t.Text); err == nil {
			q.table, q.needles = evidence.FTSWordTable, []Needle{{Kind: NeedlePhrase, Text: n}}
		}
		if err != nil {
			return nil, fmt.Errorf("term %d: %w", i+1, err)
		}
		q.match = quoteFTS(n)
		out[i] = q
	}
	return out, nil
}
