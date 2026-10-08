package sqlitefile

import (
	"math"
	"strconv"
	"strings"
)

// The CREATE TABLE and CREATE INDEX parsers read the SQL text stored in the
// schema table. The text is hostile input: the parsers are single-pass over a
// linear tokenizer (no backtracking, no recursion), skip expressions by
// counting parentheses with a depth cap, cap the number of columns, and fail
// with a short token note instead of an error. A failed parse yields no
// columns: callers fall back to the stored values.

const (
	maxParseDepth  = 256 // nesting of parentheses inside a skipped expression
	maxDefaultText = 256 // bytes of Default.Text kept
)

// Notes of a failed parse (TableDef.ParseNote): short tokens, never text
// read from the statement.
const (
	noteEmpty      = "empty"
	noteNotCreate  = "not-create"
	noteTruncated  = "truncated"
	noteUnbalanced = "unbalanced"
	noteSyntax     = "syntax"
	noteLimit      = "limit"
	noteDepth      = "depth"
	noteCTAS       = "ctas"
	noteVirtual    = "virtual"
	noteColumns    = "columns"
	notePK         = "pk"
	noteDupColumn  = "duplicate-column"
	noteStrictType = "strict-type"
)

type cparser struct {
	s       string
	lx      lexer
	la      [3]token
	nla     int
	prevEnd int
	note    string
	maxCols int
}

func newCParser(sql string, maxCols int) *cparser {
	return &cparser{s: sql, lx: lexer{s: sql}, maxCols: maxCols}
}

func (p *cparser) peek(k int) token {
	for p.nla <= k {
		p.la[p.nla] = p.lx.next()
		p.nla++
	}
	return p.la[k]
}

func (p *cparser) next() token {
	t := p.peek(0)
	copy(p.la[:], p.la[1:p.nla])
	p.nla--
	if t.kind != tkEOF {
		p.prevEnd = t.end
	}
	return t
}

// fail records the first failure note and returns false.
func (p *cparser) fail(note string) bool {
	if p.note == "" {
		p.note = note
	}
	return false
}

// failAt fails on token t: an end of input is a truncated statement, an
// unreadable token a syntax error, and anything else gets note.
func (p *cparser) failAt(t token, note string) bool {
	switch t.kind {
	case tkEOF:
		return p.fail(noteTruncated)
	case tkBad:
		return p.fail(noteSyntax)
	}
	return p.fail(note)
}

// isName reports whether t can stand for an identifier.
func isName(t token) bool { return t.kind == tkWord || t.kind == tkQuoted || t.kind == tkString }

// name consumes an identifier.
func (p *cparser) name() (string, bool) {
	t := p.next()
	if !isName(t) {
		return "", p.failAt(t, noteSyntax)
	}
	return t.text, true
}

// word consumes the bare word w.
func (p *cparser) word(w string) bool {
	t := p.next()
	if !t.isWord(w) {
		return p.failAt(t, noteSyntax)
	}
	return true
}

// skipGroup consumes a parenthesized group, the next token being its '('.
func (p *cparser) skipGroup() bool {
	if t := p.next(); !t.isPunct('(') {
		return p.failAt(t, noteSyntax)
	}
	depth := 1
	for depth > 0 {
		t := p.next()
		switch {
		case t.kind == tkEOF:
			return p.fail(noteUnbalanced)
		case t.kind == tkBad:
			return p.fail(noteSyntax)
		case t.isPunct('('):
			if depth++; depth > maxParseDepth {
				return p.fail(noteDepth)
			}
		case t.isPunct(')'):
			depth--
		}
	}
	return true
}

// qualName consumes [schema .] name.
func (p *cparser) qualName() bool {
	if _, ok := p.name(); !ok {
		return false
	}
	if p.peek(0).isPunct('.') {
		p.next()
		_, ok := p.name()
		return ok
	}
	return true
}

// ifNotExists consumes an optional IF NOT EXISTS.
func (p *cparser) ifNotExists() {
	if p.peek(0).isWord("IF") && p.peek(1).isWord("NOT") && p.peek(2).isWord("EXISTS") {
		p.next()
		p.next()
		p.next()
	}
}

// header consumes CREATE [TEMP] and returns the token that follows (not
// consumed). It fails for text that does not start like a CREATE statement.
func (p *cparser) header() (token, bool) {
	t := p.next()
	switch {
	case t.kind == tkEOF:
		return t, p.fail(noteEmpty)
	case !t.isWord("CREATE"):
		return t, p.failAt(t, noteNotCreate)
	}
	return p.peek(0), true
}

// ---- CREATE TABLE ----

var colConstraintStart = map[string]bool{
	"CONSTRAINT": true, "PRIMARY": true, "NOT": true, "NULL": true, "UNIQUE": true, "CHECK": true,
	"DEFAULT": true, "COLLATE": true, "REFERENCES": true, "GENERATED": true, "AS": true,
}

func isColConstraintStart(t token) bool {
	return t.kind == tkWord && colConstraintStart[asciiUpper(t.text)]
}

func isTableConstraintStart(t token) bool {
	return t.isWord("CONSTRAINT") || t.isWord("PRIMARY") || t.isWord("UNIQUE") || t.isWord("CHECK") || t.isWord("FOREIGN")
}

// tableParse is the state of one CREATE TABLE parse.
type tableParse struct {
	cols    []Column
	pkNames []string // the primary key's columns in key order
	pkDesc  bool     // the (single) key column is declared DESC
	pkColl  []string // the COLLATE of each key column in a table-level key clause ("" = none)
	havePK  bool
	without bool
	strict  bool
}

// parseTableSQL parses a CREATE TABLE statement. virtual is true for CREATE
// VIRTUAL TABLE (whose columns are defined by its module: ParseOK is false
// with the note "virtual"). steps is the number of tokens read.
func parseTableSQL(sql string, maxCols int) (def TableDef, virtual bool, steps int) {
	p := newCParser(sql, maxCols)
	def, virtual = p.table()
	if !def.ParseOK {
		def = TableDef{RowidAlias: -1, ParseNote: p.note}
		if virtual {
			def.ParseNote = noteVirtual
		}
		if def.ParseNote == "" {
			def.ParseNote = noteSyntax
		}
	}
	return def, virtual, p.lx.steps
}

func (p *cparser) table() (def TableDef, virtual bool) {
	t, ok := p.header()
	if !ok {
		return def, false
	}
	if t.isWord("TEMP") || t.isWord("TEMPORARY") {
		p.next()
		t = p.peek(0)
	}
	if t.isWord("VIRTUAL") {
		p.next()
		if !p.peek(0).isWord("TABLE") {
			p.failAt(p.peek(0), noteNotCreate)
			return def, false
		}
		return def, true
	}
	if !t.isWord("TABLE") {
		p.failAt(t, noteNotCreate)
		return def, false
	}
	p.next()
	p.ifNotExists()
	if !p.qualName() {
		return def, false
	}
	switch t := p.peek(0); {
	case t.isPunct('('):
	case t.isWord("AS"):
		p.fail(noteCTAS)
		return def, false
	default:
		p.failAt(t, noteSyntax)
		return def, false
	}
	st := &tableParse{}
	if !p.columnList(st) || !p.tableOptions(st) {
		return def, false
	}
	def, ok = p.finish(st)
	if !ok {
		return TableDef{}, false
	}
	return def, false
}

// columnList parses ( column-def | table-constraint {, ...} ).
func (p *cparser) columnList(st *tableParse) bool {
	p.next() // (
	inConstraints := false
	for first := true; ; first = false {
		t := p.peek(0)
		if first && t.isPunct(')') {
			return p.fail(noteColumns)
		}
		if len(st.cols) > 0 && isTableConstraintStart(t) {
			inConstraints = true
		}
		if inConstraints {
			if !p.tableConstraint(st) {
				return false
			}
		} else if !p.columnDef(st) {
			return false
		}
		switch t := p.next(); {
		case t.isPunct(','):
		case t.isPunct(')'):
			if len(st.cols) == 0 {
				return p.fail(noteColumns)
			}
			return true
		default:
			return p.failAt(t, noteSyntax)
		}
	}
}

type span struct {
	start, end int
	unquoted   string // the token text without its quotes, when quoted is set
	quoted     bool
}

func (p *cparser) columnDef(st *tableParse) bool {
	if len(st.cols) >= p.maxCols {
		return p.fail(noteColumns)
	}
	nm, ok := p.name()
	if !ok {
		return false
	}
	col := Column{Name: nm}
	var parts []span
	for {
		t := p.peek(0)
		if (t.kind == tkWord && !isColConstraintStart(t)) || t.kind == tkQuoted || t.kind == tkString {
			p.next()
			parts = append(parts, span{start: t.start, end: t.end, unquoted: t.text, quoted: t.kind == tkQuoted || t.kind == tkString})
			continue
		}
		break
	}
	if len(parts) > 0 && p.peek(0).isPunct('(') {
		start := p.peek(0).start
		if !p.skipGroup() {
			return false
		}
		parts = append(parts, span{start: start, end: p.prevEnd})
	}
	col.DeclType = p.declType(parts)
	idx := len(st.cols)
	st.cols = append(st.cols, col)
	for {
		t := p.peek(0)
		if t.isPunct(',') || t.isPunct(')') {
			return true
		}
		if !p.columnConstraint(st, idx) {
			return false
		}
	}
}

// declType returns the declared type: the source text from the first to the
// last type token, or, when comments lie in between, the tokens joined by one
// space. A quoted type token (a double-quoted, bracketed, backtick or
// single-quoted word) stands for its unquoted text, as the engine reads it, so
// a "INTEGER" PRIMARY KEY column is a rowid alias and has INTEGER affinity.
func (p *cparser) declType(parts []span) string {
	if len(parts) == 0 {
		return ""
	}
	raw := p.s[parts[0].start:parts[len(parts)-1].end]
	joined := strings.Contains(raw, "--") || strings.Contains(raw, "/*")
	var b strings.Builder
	for i, sp := range parts {
		if i > 0 {
			if joined {
				b.WriteByte(' ')
			} else {
				b.WriteString(p.s[parts[i-1].end:sp.start])
			}
		}
		if sp.quoted {
			b.WriteString(sp.unquoted)
		} else {
			b.WriteString(p.s[sp.start:sp.end])
		}
	}
	return b.String()
}

// conflict consumes an optional ON CONFLICT action.
func (p *cparser) conflict() bool {
	if p.peek(0).isWord("ON") && p.peek(1).isWord("CONFLICT") {
		p.next()
		p.next()
		t := p.next()
		if t.kind != tkWord {
			return p.failAt(t, noteSyntax)
		}
	}
	return true
}

func (p *cparser) columnConstraint(st *tableParse, idx int) bool {
	col := &st.cols[idx]
	t := p.peek(0)
	if t.kind != tkWord {
		return p.failAt(t, noteSyntax)
	}
	switch asciiUpper(t.text) {
	case "CONSTRAINT":
		p.next()
		_, ok := p.name()
		return ok
	case "PRIMARY":
		p.next()
		if !p.word("KEY") {
			return false
		}
		desc := false
		switch {
		case p.peek(0).isWord("ASC"):
			p.next()
		case p.peek(0).isWord("DESC"):
			p.next()
			desc = true
		}
		if !p.conflict() {
			return false
		}
		if p.peek(0).isWord("AUTOINCREMENT") {
			p.next()
		}
		if st.havePK {
			return p.fail(notePK)
		}
		st.havePK, st.pkNames, st.pkDesc = true, []string{col.Name}, desc
		return true
	case "NOT":
		p.next()
		if !p.word("NULL") || !p.conflict() {
			return false
		}
		col.NotNull = true
		return true
	case "NULL", "UNIQUE":
		p.next()
		return p.conflict()
	case "CHECK":
		p.next()
		return p.skipGroup()
	case "DEFAULT":
		p.next()
		return p.defaultClause(col)
	case "COLLATE":
		p.next()
		nm, ok := p.name()
		col.Collation = nm
		return ok
	case "REFERENCES":
		p.next()
		return p.references()
	case "GENERATED":
		p.next()
		return p.word("ALWAYS") && p.word("AS") && p.generated(col)
	case "AS":
		p.next()
		return p.generated(col)
	}
	return p.fail(noteSyntax)
}

// generated parses ( expr ) [STORED | VIRTUAL].
func (p *cparser) generated(col *Column) bool {
	if !p.peek(0).isPunct('(') {
		return p.failAt(p.peek(0), noteSyntax)
	}
	if !p.skipGroup() {
		return false
	}
	col.Generated = GenVirtual
	switch {
	case p.peek(0).isWord("STORED"):
		p.next()
		col.Generated = GenStored
	case p.peek(0).isWord("VIRTUAL"):
		p.next()
	}
	return true
}

// references parses the rest of a REFERENCES clause: table [(cols)] and the
// ON DELETE/UPDATE, MATCH, DEFERRABLE and INITIALLY clauses.
func (p *cparser) references() bool {
	if _, ok := p.name(); !ok {
		return false
	}
	if p.peek(0).isPunct('(') && !p.skipGroup() {
		return false
	}
	for {
		t := p.peek(0)
		switch {
		case t.isWord("ON") && !p.peek(1).isWord("CONFLICT"):
			p.next()
			if t := p.next(); !t.isWord("DELETE") && !t.isWord("UPDATE") && !t.isWord("INSERT") {
				return p.failAt(t, noteSyntax)
			}
			switch a := p.next(); {
			case a.isWord("SET"):
				if b := p.next(); !b.isWord("NULL") && !b.isWord("DEFAULT") {
					return p.failAt(b, noteSyntax)
				}
			case a.isWord("CASCADE"), a.isWord("RESTRICT"):
			case a.isWord("NO"):
				if !p.word("ACTION") {
					return false
				}
			default:
				return p.failAt(a, noteSyntax)
			}
		case t.isWord("MATCH"):
			p.next()
			if _, ok := p.name(); !ok {
				return false
			}
		case t.isWord("NOT") && p.peek(1).isWord("DEFERRABLE"):
			p.next()
			p.next()
		case t.isWord("DEFERRABLE"):
			p.next()
		case t.isWord("INITIALLY"):
			p.next()
			if b := p.next(); !b.isWord("DEFERRED") && !b.isWord("IMMEDIATE") {
				return p.failAt(b, noteSyntax)
			}
		default:
			return true
		}
	}
}

// defaultClause parses what follows DEFAULT.
func (p *cparser) defaultClause(col *Column) bool {
	t := p.peek(0)
	start := t.start
	expr := func() bool {
		col.Default = Default{Kind: DefaultExpr, Text: clipMsg(p.s[start:p.prevEnd])}
		return true
	}
	literal := func(v Value) bool {
		col.Default = Default{Kind: DefaultLiteral, Value: v, Text: clipMsg(p.s[start:p.prevEnd])}
		return true
	}
	switch {
	case t.isPunct('('):
		if !p.skipGroup() {
			return false
		}
		return expr()
	case t.isPunct('+') || t.isPunct('-'):
		p.next()
		n := p.peek(0)
		if n.kind != tkNumber {
			if n.kind == tkEOF || n.kind == tkBad {
				return p.failAt(n, noteSyntax)
			}
			p.next()
			return expr() // a signed term that is not a number: not a literal
		}
		p.next()
		if v, ok := numberValue(n.text, t.isPunct('-')); ok {
			return literal(v)
		}
		return expr()
	case t.kind == tkNumber:
		p.next()
		if v, ok := numberValue(t.text, false); ok {
			return literal(v)
		}
		return expr()
	case t.kind == tkString || t.kind == tkQuoted:
		p.next()
		return literal(textValue(t.text))
	case t.kind == tkWord:
		p.next()
		switch asciiUpper(t.text) {
		case "NULL":
			return literal(Value{})
		case "TRUE":
			return literal(Value{Kind: KindInt, Int: 1})
		case "FALSE":
			return literal(Value{Kind: KindInt, Int: 0})
		case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
			return expr()
		case "X":
			if n := p.peek(0); n.kind == tkString && n.start == t.end {
				p.next()
				if b, ok := hexBytes(n.text); ok {
					return literal(Value{Kind: KindBlob, Bytes: b, Len: int64(len(b))})
				}
				return expr()
			}
		}
		return literal(textValue(t.text)) // a bare identifier is read as a string
	}
	return p.failAt(t, noteSyntax)
}

func textValue(s string) Value {
	return Value{Kind: KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: EncUTF8}
}

func hexBytes(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		hi, ok1 := hexVal(s[2*i])
		lo, ok2 := hexVal(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// numberValue reads a numeric literal as the engine does: hexadecimal
// integers wrap to 64 bits, a decimal integer that does not fit is a real,
// and the text must be a plain number (no underscores, no "inf" or "nan").
func numberValue(text string, neg bool) (Value, bool) {
	if isHexNumber(text) {
		digits := text[2:]
		if len(digits) == 0 || len(digits) > 16 {
			return Value{}, false
		}
		u, err := strconv.ParseUint(digits, 16, 64)
		if err != nil {
			return Value{}, false
		}
		n := int64(u)
		if neg {
			n = -n
		}
		return Value{Kind: KindInt, Int: n}, true
	}
	allDigits := true
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !isDigit(c) {
			allDigits = false
			if c != '.' && c != 'e' && c != 'E' && c != '+' && c != '-' {
				return Value{}, false
			}
		}
	}
	if allDigits {
		if u, err := strconv.ParseUint(text, 10, 64); err == nil {
			switch {
			case u <= math.MaxInt64:
				n := int64(u)
				if neg {
					n = -n
				}
				return Value{Kind: KindInt, Int: n}, true
			case neg && u == 1<<63:
				return Value{Kind: KindInt, Int: math.MinInt64}, true
			}
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return Value{}, false
	}
	if neg {
		f = -f
	}
	return Value{Kind: KindFloat, Float: f}, true
}

// tableConstraint parses one table constraint.
func (p *cparser) tableConstraint(st *tableParse) bool {
	t := p.peek(0)
	if t.isWord("CONSTRAINT") {
		p.next()
		if _, ok := p.name(); !ok {
			return false
		}
		t = p.peek(0)
	}
	switch {
	case t.isWord("PRIMARY"):
		p.next()
		if !p.word("KEY") {
			return false
		}
		return p.primaryKeyList(st)
	case t.isWord("UNIQUE"):
		p.next()
		return p.skipGroup() && p.conflict()
	case t.isWord("CHECK"):
		p.next()
		return p.skipGroup()
	case t.isWord("FOREIGN"):
		p.next()
		return p.word("KEY") && p.skipGroup() && p.word("REFERENCES") && p.references()
	}
	return p.failAt(t, noteSyntax)
}

// primaryKeyList parses ( name [COLLATE c] [ASC|DESC] [AUTOINCREMENT] {, ...} ).
func (p *cparser) primaryKeyList(st *tableParse) bool {
	if t := p.next(); !t.isPunct('(') {
		return p.failAt(t, noteSyntax)
	}
	var names, colls []string
	for {
		t := p.next()
		if !isName(t) { // an expression in a key; a string literal names a column, as the engine reads it
			return p.failAt(t, notePK)
		}
		names = append(names, t.text)
		colls = append(colls, "")
		for {
			switch n := p.peek(0); {
			case n.isWord("COLLATE"):
				p.next()
				nm, ok := p.name()
				if !ok {
					return false
				}
				colls[len(colls)-1] = nm
				continue
			case n.isWord("ASC"):
				p.next()
				continue
			case n.isWord("DESC"):
				p.next()
				continue
			case n.isWord("AUTOINCREMENT"):
				p.next()
				continue
			}
			break
		}
		switch t := p.next(); {
		case t.isPunct(','):
			if len(names) >= p.maxCols {
				return p.fail(noteColumns)
			}
			continue
		case t.isPunct(')'):
			if !p.conflict() {
				return false
			}
			if st.havePK {
				return p.fail(notePK)
			}
			st.havePK, st.pkNames, st.pkDesc, st.pkColl = true, names, false, colls // the engine ignores DESC in a table constraint (TestSchemaMatchesEngine)
			return true
		default:
			return p.failAt(t, notePK)
		}
	}
}

// tableOptions parses what follows the closing parenthesis: WITHOUT ROWID and
// STRICT in any order, comma separated, and an optional semicolon.
func (p *cparser) tableOptions(st *tableParse) bool {
	for {
		t := p.peek(0)
		switch {
		case t.kind == tkEOF:
			return true
		case t.isPunct(';'):
			p.next()
			if e := p.peek(0); e.kind != tkEOF {
				return p.failAt(e, noteSyntax)
			}
			return true
		case t.isWord("WITHOUT"):
			p.next()
			if !p.word("ROWID") {
				return false
			}
			st.without = true
		case t.isWord("STRICT"):
			p.next()
			st.strict = true
		default:
			return p.failAt(t, noteSyntax)
		}
		if p.peek(0).isPunct(',') { // another option must follow
			p.next()
			if f := p.peek(0); !f.isWord("WITHOUT") && !f.isWord("STRICT") {
				return p.failAt(f, noteSyntax)
			}
		}
	}
}

var strictTypes = map[string]bool{"int": true, "integer": true, "real": true, "text": true, "blob": true, "any": true}

// finish resolves the key, the affinities and the record layout.
func (p *cparser) finish(st *tableParse) (TableDef, bool) {
	cols := st.cols
	// The engine refuses duplicate column names (ASCII case-insensitive) and, in a
	// STRICT table, any declared type but INT, INTEGER, REAL, TEXT, BLOB or ANY.
	names := make(map[string]struct{}, len(cols))
	for i := range cols {
		key := asciiLower(cols[i].Name)
		if _, dup := names[key]; dup {
			return TableDef{}, p.fail(noteDupColumn)
		}
		names[key] = struct{}{}
		if st.strict && !strictTypes[asciiLower(cols[i].DeclType)] {
			return TableDef{}, p.fail(noteStrictType)
		}
	}
	def := TableDef{Columns: cols, WithoutRowid: st.without, Strict: st.strict, RowidAlias: -1, ParseOK: true}
	pkIdx := make([]int, 0, len(st.pkNames))
	pkColl := make([]string, 0, len(st.pkNames)) // the collation of each entry of pkIdx
	used := make([]bool, len(cols))
	for n, nm := range st.pkNames {
		found := -1
		for i := range cols {
			if asciiEqualFold(cols[i].Name, nm) {
				found = i
				break
			}
		}
		if found < 0 || cols[found].Generated != GenNone {
			return def, p.fail(notePK)
		}
		coll := ""
		if n < len(st.pkColl) {
			coll = st.pkColl[n]
		}
		if used[found] {
			// A column named twice is a key column twice (the engine keys the index
			// by (a, a) under each collation). With one collation both keys are one
			// key; with two they order and compare rows differently from either
			// alone, so the identity of a row is not derived: the table stays unparsed.
			for k, i := range pkIdx {
				if i == found && !asciiEqualFold(pkColl[k], coll) {
					return def, p.fail(notePK)
				}
			}
			continue
		}
		used[found] = true
		pkIdx = append(pkIdx, found)
		pkColl = append(pkColl, coll)
	}
	if st.without && len(pkIdx) == 0 {
		return def, p.fail(notePK)
	}
	for k, i := range pkIdx {
		cols[i].PKOrdinal = k + 1
		cols[i].KeyCollation = pkColl[k]
	}
	for i := range cols {
		c := &cols[i]
		c.Affinity = AffinityOf(c.DeclType)
		if st.strict && asciiEqualFold(c.DeclType, "ANY") {
			c.Affinity = AffBlob // a STRICT column of type ANY keeps what it is given
		}
	}
	if !st.without && len(pkIdx) == 1 && !st.pkDesc && asciiEqualFold(cols[pkIdx[0]].DeclType, "INTEGER") {
		def.RowidAlias = pkIdx[0]
	}
	// A key column is NOT NULL in a WITHOUT ROWID table, and in a STRICT table
	// unless it is the rowid alias (table_xinfo of the engine).
	for _, i := range pkIdx {
		if st.without || (st.strict && i != def.RowidAlias) {
			cols[i].NotNull = true
		}
	}
	rec := 0
	if st.without { // the key columns come first, in key order, then the rest as declared
		for _, i := range pkIdx {
			cols[i].RecordIndex = rec
			rec++
		}
	}
	for i := range cols {
		switch {
		case cols[i].Generated == GenVirtual:
			cols[i].RecordIndex = -1
		case st.without && cols[i].PKOrdinal > 0:
		default:
			cols[i].RecordIndex = rec
			rec++
		}
	}
	def.StoredColumns = rec
	return def, true
}

// ---- CREATE INDEX ----

// parseIndexSQL parses a CREATE INDEX statement. steps is the number of
// tokens read.
func parseIndexSQL(sql string, maxCols int) (def IndexDef, steps int) {
	p := newCParser(sql, maxCols)
	def, ok := p.index()
	if !ok {
		def = IndexDef{}
	}
	return def, p.lx.steps
}

func (p *cparser) index() (IndexDef, bool) {
	var def IndexDef
	t, ok := p.header()
	if !ok {
		return def, false
	}
	if t.isWord("UNIQUE") {
		p.next()
		def.Unique = true
		t = p.peek(0)
	}
	if !t.isWord("INDEX") {
		return def, p.failAt(t, noteNotCreate)
	}
	p.next()
	p.ifNotExists()
	if !p.qualName() || !p.word("ON") {
		return def, false
	}
	tbl, ok := p.name()
	if !ok {
		return def, false
	}
	def.Table = tbl
	if t := p.next(); !t.isPunct('(') {
		return def, p.failAt(t, noteSyntax)
	}
	for first := true; ; first = false {
		if first && p.peek(0).isPunct(')') {
			return def, p.fail(noteColumns)
		}
		if len(def.Columns) >= p.maxCols {
			return def, p.fail(noteColumns)
		}
		col, ok := p.indexColumn()
		if !ok {
			return def, false
		}
		def.Columns = append(def.Columns, col)
		switch t := p.next(); {
		case t.isPunct(','):
		case t.isPunct(')'):
			return p.indexTail(def)
		default:
			return def, p.failAt(t, noteSyntax)
		}
	}
}

// indexColumn parses name | expr, then [COLLATE c] [ASC | DESC].
func (p *cparser) indexColumn() (IndexColumn, bool) {
	var col IndexColumn
	t, n := p.peek(0), p.peek(1)
	simple := (t.kind == tkWord || t.kind == tkQuoted) &&
		(n.isPunct(',') || n.isPunct(')') || n.isWord("COLLATE") || n.isWord("ASC") || n.isWord("DESC"))
	if simple {
		p.next()
		col.Name = t.text
		for {
			switch m := p.peek(0); {
			case m.isWord("COLLATE"):
				p.next()
				nm, ok := p.name()
				if !ok {
					return col, false
				}
				col.Collation = nm
				continue
			case m.isWord("ASC"):
				p.next()
				continue
			case m.isWord("DESC"):
				p.next()
				col.Desc = true
				continue
			}
			return col, true
		}
	}
	// An expression: skip to the next top-level comma or closing parenthesis,
	// picking up a COLLATE, ASC or DESC at the top level.
	col.Expr = true
	depth, count := 0, 0
	for {
		t := p.peek(0)
		switch {
		case t.kind == tkEOF:
			return col, p.fail(noteTruncated)
		case t.kind == tkBad:
			return col, p.fail(noteSyntax)
		case depth == 0 && (t.isPunct(',') || t.isPunct(')')):
			if count == 0 {
				return col, p.fail(noteSyntax)
			}
			return col, true
		case t.isPunct('('):
			if depth++; depth > maxParseDepth {
				return col, p.fail(noteDepth)
			}
		case t.isPunct(')'):
			depth--
		case depth == 0 && t.isWord("COLLATE"):
			p.next()
			nm, ok := p.name()
			if !ok {
				return col, false
			}
			col.Collation = nm
			count++
			continue
		case depth == 0 && t.isWord("DESC"):
			col.Desc = true
		}
		p.next()
		count++
	}
}

// indexTail handles what follows the key list: nothing, a semicolon, or a
// WHERE clause (read to the end and not interpreted).
func (p *cparser) indexTail(def IndexDef) (IndexDef, bool) {
	t := p.peek(0)
	switch {
	case t.kind == tkEOF:
	case t.isPunct(';'):
		p.next()
		if e := p.peek(0); e.kind != tkEOF {
			return def, p.failAt(e, noteSyntax)
		}
	case t.isWord("WHERE"):
		p.next()
		def.Partial = true
		for {
			e := p.next()
			if e.kind == tkEOF {
				break
			}
			if e.kind == tkBad {
				return def, p.fail(noteSyntax)
			}
		}
	default:
		return def, p.failAt(t, noteSyntax)
	}
	def.ParseOK = true
	return def, true
}

// sqlNames reads the head of a CREATE statement: the kind of object (TABLE,
// INDEX, VIEW or TRIGGER, upper case), its own name (the part after a schema
// prefix) and, for an index or a trigger, the table it is on, as the
// statement spells them with the quotes removed. ok is false when the
// statement cannot be read that far; the caller then checks nothing.
func sqlNames(sql string) (kind, name, on string, ok bool) {
	p := newCParser(sql, 0)
	t, hok := p.header()
	if !hok {
		return "", "", "", false
	}
	for t.isWord("TEMP") || t.isWord("TEMPORARY") || t.isWord("UNIQUE") || t.isWord("VIRTUAL") {
		p.next()
		t = p.peek(0)
	}
	if t.kind != tkWord {
		return "", "", "", false
	}
	switch kind = asciiUpper(t.text); kind {
	case "TABLE", "INDEX", "VIEW", "TRIGGER":
	default:
		return "", "", "", false
	}
	p.next()
	p.ifNotExists()
	qual := func() (string, bool) {
		n, ok := p.name()
		if !ok {
			return "", false
		}
		if p.peek(0).isPunct('.') {
			p.next()
			return p.name()
		}
		return n, true
	}
	if name, ok = qual(); !ok {
		return "", "", "", false
	}
	switch kind {
	case "INDEX":
		if !p.word("ON") {
			return "", "", "", false
		}
	case "TRIGGER":
		// BEFORE | AFTER | INSTEAD OF, the event and an UPDATE OF column list
		// come first; the first bare ON is the table's.
		const maxScan = 4096
		for i := 0; ; i++ {
			t := p.next()
			if t.kind == tkEOF || t.kind == tkBad || i > maxScan {
				return "", "", "", false
			}
			if t.isWord("ON") {
				break
			}
		}
	default:
		return kind, name, "", true
	}
	if on, ok = qual(); !ok {
		return "", "", "", false
	}
	return kind, name, on, true
}
