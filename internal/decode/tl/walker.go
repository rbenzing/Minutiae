package tl

import (
	"errors"
	"fmt"
	"strconv"
)

// maxDetail caps the quoted panic value kept in Error.Detail (as typedstream.guard).
const maxDetail = 200

// Named is one decoded field: its dotted path (`fwd_from.from_id`, `entities[2].offset`), its
// kind and the value in the member the kind names (table.go). Bytes is a sub-slice of the
// blob, never a copy, and is nil for KSecret (spec section 9). A KObject carries the type it
// was looked up under in Type (the field's Of, or the type of a union that held its ID; plan
// 4I4c W1), so a caller resolves its class by Key{Type, ID}, never by the ID alone.
type Named struct {
	Path  string
	Kind  Kind
	Int   int64
	Float float64
	Bool  bool
	Bytes []byte
	Len   int
	ID    uint32
	Type  string
	Class string
}

// Stop kinds (plan 4I4-P10 maps each to a blob state).
const (
	// StopUnknownConstructor: an ID the table lacks for the expected type (Err is an
	// *UnknownConstructorError).
	StopUnknownConstructor = "unknown-constructor"
	// StopOpaque: an object whose body the table does not pin (Err nil).
	StopOpaque = "opaque"
	// StopPrefix: the last listed field of a prefix body was read (Err nil).
	StopPrefix = "prefix"
	// StopTruncated: ErrTruncated (also a declared length or count past the end, T7).
	StopTruncated = "truncated"
	// StopMalformed: ErrBadBool, ErrBadVector or ErrInternal.
	StopMalformed = "malformed"
	// StopCap: ErrVectorCount, ErrDepth, ErrSteps or ErrTooLarge.
	StopCap = "cap"
	// StopBudget: ErrNoBudget.
	StopBudget = "budget"
)

// Stop says where and why a decode ended before the end of the top object: the offset and
// path of the field or object where it ended, the type expected or being read there, and
// the constructor ID met (the unknown or opaque one) or being read.
type Stop struct {
	Kind   string
	Err    error
	Offset int
	Path   string
	Type   string
	ID     uint32
}

// UnknownConstructorError is an ID the table lacks for the expected type, at the offset
// where the ID begins.
type UnknownConstructorError struct {
	ID     uint32
	Offset int
	Path   string
	Type   string
}

// Error names the ID, the expected type, the offset and the path.
func (e *UnknownConstructorError) Error() string {
	return fmt.Sprintf("tl: unknown constructor %#08x for type %q at offset %d (path %q)", e.ID, e.Type, e.Offset, e.Path)
}

// Result is what a decode read. Fields hold everything decoded before Stop, and Stop is nil
// when the top object was read whole; Flags are the encoding facts met (reader.go); End is the offset
// after the last field read (the end of the top object when Stop is nil: where an app-local
// trailer starts); Steps is the reader's step count at return.
type Result struct {
	Top    uint32
	Fields []Named
	Stop   *Stop
	Flags  []string
	End    int
	Steps  int
}

// FieldOverhead is the fixed part of what one Named charges to the budget: the Named itself
// (120 bytes on a 64-bit platform since plan 4I4c added Named.Type, ruling C5; 104 before)
// and its share of the Fields slice, rounded up. Each Named also charges its path's length
// (F8, Task 3 review I1). The Fields slice's spare capacity and the allocator's size classes
// stay uncharged (docs/limitations/parsers.md, Decoders).
const FieldOverhead = 128

// cost is what n charges to the budget.
func cost(n *Named) int64 { return FieldOverhead + int64(len(n.Path)) }

// Charged is what the decode charged to the budget for Fields (FieldOverhead and the path's
// length per field): the caller frees exactly this when it is done with the result.
func (r Result) Charged() int64 {
	var total int64
	for i := range r.Fields {
		total += cost(&r.Fields[i])
	}
	return total
}

// Flags word names a field's Flags may name (TLRPC: flags, then flags2).
const (
	wordFlags  = "flags"
	wordFlags2 = "flags2"
)

// Decode reads the object of type typ at the start of b by the table t (spec section 7.2).
// It reads the top ID; an ID t lacks under typ (Key{typ, ID}) is StopUnknownConstructor with
// Top set and no field. Fields are read in table order; a gated field only when its bit is
// set in the flags word read before it in the same object. The walker never skips: an
// object it cannot read ends the whole decode, and everything read before it is returned.
//
// Every Named appended charges FieldOverhead and its path's length to budget (a refusal, or
// a nil budget, is StopBudget). Decode never frees: the caller frees Result.Charged() when it
// is done with the result. A panic is recovered into StopMalformed with ErrInternal and a
// Detail "panic: " and the quoted value, apart from a table defect ("table: ...").
func Decode(b []byte, typ string, t Table, budget Budget) (res Result) {
	w := &walker{t: t, budget: budget}
	defer func() {
		if p := recover(); p != nil {
			off := 0
			if w.r != nil {
				off = w.r.Offset()
			}
			msg := strconv.QuoteToASCII(fmt.Sprint(p))
			if len(msg) > maxDetail {
				msg = msg[:maxDetail]
			}
			w.stop = &Stop{
				Kind: StopMalformed, Err: &Error{Err: ErrInternal, Offset: off, Path: w.path, Detail: "panic: " + msg},
				Offset: off, Path: w.path, Type: w.typ, ID: w.id,
			}
			res = w.result()
		}
	}()
	w.decode(b, typ)
	return w.result()
}

// walker is the state of one Decode.
type walker struct {
	r      *Reader
	t      Table
	budget Budget
	top    uint32
	fields []Named
	stop   *Stop
	end    int
	// where: the path being read and the type and ID of the object it belongs to, for a
	// recovered panic.
	path string
	typ  string
	id   uint32
}

func (w *walker) result() Result {
	res := Result{Top: w.top, Fields: w.fields, Stop: w.stop, End: w.end}
	if w.r != nil {
		res.Flags, res.Steps = w.r.Flags(), w.r.Steps()
	}
	return res
}

func (w *walker) decode(b []byte, typ string) {
	r, err := NewReader(b)
	if err != nil {
		w.fail(err, "", typ, 0)
		return
	}
	w.r = r
	id, err := r.Uint32()
	if err != nil {
		w.fail(err, "", typ, 0)
		return
	}
	w.top = id
	c, ok := w.lookup(id, typ, "", "", 0)
	if !ok {
		return
	}
	w.end = r.Offset()
	if w.body(c, "", 1) {
		w.end = r.Offset()
	}
}

// lookup finds id under the type typ, or, for a union element (alt set), under alt when typ
// lacks it (plan 4I4c P5: the first type that holds the ID decides, whatever its body), or
// stops: unknown-constructor (naming "typ|alt" for a union), or opaque for a body the table
// does not pin. off is where the ID began.
func (w *walker) lookup(id uint32, typ, alt, path string, off int) (Constructor, bool) {
	c, ok := w.t[Key{Type: typ, ID: id}]
	if !ok && alt != "" {
		if c, ok = w.t[Key{Type: alt, ID: id}]; ok {
			typ = alt
		}
	}
	if !ok {
		want := expected(typ, alt)
		w.stop = &Stop{
			Kind: StopUnknownConstructor, Err: &UnknownConstructorError{ID: id, Offset: off, Path: path, Type: want},
			Offset: off, Path: path, Type: want, ID: id,
		}
		return Constructor{}, false
	}
	if c.Body == BodyOpaque {
		w.stop = &Stop{Kind: StopOpaque, Offset: off, Path: path, Type: typ, ID: id}
		return Constructor{}, false
	}
	// The key read is what the object is and what every later Stop names, never a differing
	// Constructor.ID or Constructor.Type (review M6, ruling C1).
	c.ID, c.Type = id, typ
	return c, true
}

// expected is the type a field expects: typ, or "typ|alt" for a union element.
func expected(typ, alt string) string {
	if alt == "" {
		return typ
	}
	return typ + "|" + alt
}

// fieldDefect names what is wrong with the union type or the class of f for its kind (plan
// 4I4c P6), "" when nothing is: a union type (Alt) belongs to a KVector only, with a first
// type and a second one that differs; a class belongs to no vector of objects or strings, and
// the class of a bare id vector is one of user, chat, channel and unknown (T5).
func fieldDefect(f *Field) string {
	switch {
	case f.Alt != "" && f.Kind != KVector:
		return "union type on a field that is not a vector of objects"
	case f.Alt != "" && f.Of == "":
		return "union type without a first type"
	case f.Alt != "" && f.Alt == f.Of:
		return "union of one type twice"
	case f.Class != "" && (f.Kind == KVector || f.Kind == KVecString):
		return "class on a vector of objects or strings"
	case f.Class != "" && (f.Kind == KVecInt32 || f.Kind == KVecInt64) && !idClass(f.Class):
		return "unknown class on an id vector"
	}
	return ""
}

// idClass says c is an identity class (T5).
func idClass(c string) bool {
	switch c {
	case "user", "chat", "channel", "unknown":
		return true
	}
	return false
}

// defect stops on a table defect: ErrInternal with the detail "table: " and what.
func (w *walker) defect(what, path string, c *Constructor) {
	w.fail(&Error{Err: ErrInternal, Offset: w.r.Offset(), Detail: "table: " + what}, path, c.Type, c.ID)
}

// fail stops on err (an *Error of the Reader, or a sentinel) with the field path filled in.
func (w *walker) fail(err error, path, typ string, id uint32) {
	var te *Error
	if !errors.As(err, &te) {
		off := 0
		if w.r != nil {
			off = w.r.Offset()
		}
		te = &Error{Err: err, Offset: off}
	}
	te.Path = path
	w.stop = &Stop{Kind: stopKind(te.Err), Err: te, Offset: te.Offset, Path: path, Type: typ, ID: id}
}

// stopKind maps a sentinel to its stop kind (4I4-P10).
func stopKind(err error) string {
	switch err {
	case ErrTruncated:
		return StopTruncated
	case ErrVectorCount, ErrDepth, ErrSteps, ErrTooLarge:
		return StopCap
	case ErrNoBudget:
		return StopBudget
	default: // ErrBadBool, ErrBadVector, ErrInternal
		return StopMalformed
	}
}

// add charges and appends one Named read from off; a refusal stops at off with ErrNoBudget.
func (w *walker) add(n Named, off int, c *Constructor) bool {
	if w.budget == nil || w.budget.Alloc(cost(&n)) != nil {
		w.stop = &Stop{
			Kind: StopBudget, Err: &Error{Err: ErrNoBudget, Offset: off, Path: n.Path},
			Offset: off, Path: n.Path, Type: c.Type, ID: c.ID,
		}
		return false
	}
	w.fields = append(w.fields, n)
	w.end = w.r.Offset()
	return true
}

// flagWords holds the flags words of one object as they are read.
type flagWords struct {
	v   [2]uint32
	set [2]bool
}

func wordIndex(name string) int {
	switch name {
	case wordFlags:
		return 0
	case wordFlags2:
		return 1
	}
	return -1
}

// body reads the fields of c, an object at path and nesting depth; false when it stopped.
// A prefix body stops after its last listed field.
func (w *walker) body(c Constructor, path string, depth int) bool {
	var words flagWords
	for i := range c.Fields {
		f := &c.Fields[i]
		fp := join(path, f.Name)
		w.path, w.typ, w.id = fp, c.Type, c.ID
		if f.Flags != "" {
			k := wordIndex(f.Flags)
			switch {
			case k < 0:
				w.defect("gate names no flags word", fp, &c)
				return false
			case !words.set[k]:
				w.defect("gated field before its flags word", fp, &c)
				return false
			case f.Bit < 0 || f.Bit > 31:
				w.defect("flag bit out of range", fp, &c)
				return false
			}
			if words.v[k]&(1<<uint(f.Bit)) == 0 {
				continue
			}
		}
		if !w.field(&c, f, fp, depth, &words) {
			return false
		}
	}
	if c.Body == BodyPrefix {
		w.stop = &Stop{Kind: StopPrefix, Offset: w.r.Offset(), Path: path, Type: c.Type, ID: c.ID}
		return false
	}
	return true
}

// field reads one field f of the object c at path fp.
func (w *walker) field(c *Constructor, f *Field, fp string, depth int, words *flagWords) bool {
	r := w.r
	off := r.Offset()
	n := Named{Path: fp, Kind: f.Kind}
	if what := fieldDefect(f); what != "" {
		w.defect(what, fp, c)
		return false
	}
	var err error
	switch f.Kind {
	case KInt32:
		var v int32
		v, err = r.Int32()
		n.Int = int64(v)
	case KInt64:
		n.Int, err = r.Int64()
	case KID32:
		var v int32
		v, err = r.Int32()
		n.Int, n.Class = int64(v), f.Class
	case KID64:
		n.Int, err = r.Int64()
		n.Class = f.Class
	case KDouble:
		n.Float, err = r.Double()
	case KBool:
		n.Bool, err = r.Bool()
	case KBytes, KString:
		n.Bytes, err = r.Bytes()
		n.Len = len(n.Bytes)
	case KSecret:
		var v []byte
		v, err = r.Bytes()
		n.Len = len(v)
	case KFlags:
		k := wordIndex(f.Name)
		if k < 0 {
			w.defect("flags word not named flags or flags2", fp, c)
			return false
		}
		var v uint32
		if v, err = r.Uint32(); err == nil {
			words.v[k], words.set[k] = v, true
			n.Int = int64(v)
			if c.Body == BodyFull && v&^gateMask(c, f.Name) != 0 {
				r.flags |= flagUnknownFlagBits
			}
		}
	case KTrue:
		if f.Flags == "" {
			w.defect("flag-only true without a gate", fp, c)
			return false
		}
		n.Bool = true
	case KObject:
		return w.object(f.Of, "", fp, off, depth)
	case KVector, KVecInt32, KVecInt64, KVecString:
		return w.vector(c, f, fp, depth)
	default:
		w.defect("unknown field kind", fp, c)
		return false
	}
	if err != nil {
		w.fail(err, fp, c.Type, c.ID)
		return false
	}
	return w.add(n, off, c)
}

// gateMask is the bits of the flags word named word that gate a field of c.
func gateMask(c *Constructor, word string) uint32 {
	var m uint32
	for _, f := range c.Fields {
		if f.Flags == word && f.Bit >= 0 && f.Bit <= 31 {
			m |= 1 << uint(f.Bit)
		}
	}
	return m
}

// object reads a boxed object of the type typ (or, for a union element, alt) at path, one
// level below depth. Its Named carries the type it was looked up under.
func (w *walker) object(typ, alt, path string, off, depth int) bool {
	if depth+1 > MaxDepth {
		w.fail(ErrDepth, path, expected(typ, alt), 0)
		return false
	}
	id, err := w.r.Uint32()
	if err != nil {
		w.fail(err, path, expected(typ, alt), 0)
		return false
	}
	c, ok := w.lookup(id, typ, alt, path, off)
	if !ok {
		return false
	}
	if !w.add(Named{Path: path, Kind: KObject, ID: id, Type: c.Type}, off, &c) {
		return false
	}
	return w.body(c, path, depth+1)
}

// vector reads a vector field: the header, then boxed objects (KVector, by Of, else Alt),
// bare ints (KInt32 or KInt64; KID32 or KID64 with the class of the field when it has one) or
// bare TL strings (KVecString, each a KString). Every element takes at least 4 bytes, 8 for
// an int64, and the count is checked against that minimum before the first element (plan
// 4I4c W3).
func (w *walker) vector(c *Constructor, f *Field, fp string, depth int) bool {
	r := w.r
	off := r.Offset()
	minElem := 4
	if f.Kind == KVecInt64 {
		minElem = 8
	}
	count, err := r.VectorHeader(minElem)
	if err != nil {
		w.fail(err, fp, c.Type, c.ID)
		return false
	}
	if !w.add(Named{Path: fp, Kind: f.Kind, Len: count}, off, c) {
		return false
	}
	for i := range count {
		ep := fp + "[" + strconv.Itoa(i) + "]"
		w.path, w.typ, w.id = ep, c.Type, c.ID
		eoff := r.Offset()
		if f.Kind == KVector {
			if !w.object(f.Of, f.Alt, ep, eoff, depth) {
				return false
			}
			continue
		}
		n := Named{Path: ep, Class: f.Class}
		switch f.Kind {
		case KVecString:
			n.Kind = KString
			n.Bytes, err = r.String()
			n.Len = len(n.Bytes)
		case KVecInt32:
			n.Kind = KInt32
			if f.Class != "" {
				n.Kind = KID32
			}
			var v int32
			v, err = r.Int32()
			n.Int = int64(v)
		default:
			n.Kind = KInt64
			if f.Class != "" {
				n.Kind = KID64
			}
			n.Int, err = r.Int64()
		}
		if err != nil {
			w.fail(err, ep, c.Type, c.ID)
			return false
		}
		if !w.add(n, eoff, c) {
			return false
		}
	}
	return true
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
