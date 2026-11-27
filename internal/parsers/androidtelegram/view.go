package androidtelegram

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// view reads a decode result through the field paths of the table (`id`, `fwd_from.from_id`, `entities[0].offset`):
// at maps a path to the index of its first field in r.Fields. A path absent from the result was not read: the field
// is gated off, lies after the stop, or is not in the constructor; the caller tells these apart through the stop and
// the flags (spec 8.4), never by reading an absence as a value.
type view struct {
	r   tl.Result
	at  map[string]int
	tab tl.Table // the constructor table the result was decoded with (nil: class names are not asked)
}

// newView indexes the fields of r by path (the first field of each path).
func newView(r tl.Result) view {
	v := view{r: r, at: make(map[string]int, len(r.Fields))}
	for i := range r.Fields {
		if _, dup := v.at[r.Fields[i].Path]; !dup {
			v.at[r.Fields[i].Path] = i
		}
	}
	return v
}

// Named is the field read at path.
func (v view) Named(path string) (tl.Named, bool) {
	i, ok := v.at[path]
	if !ok {
		return tl.Named{}, false
	}
	return v.r.Fields[i], true
}

// Has says a field was read at path.
func (v view) Has(path string) bool {
	_, ok := v.at[path]
	return ok
}

// Int is an integer field (int32, int64, an inline id or a flags word) read at path.
func (v view) Int(path string) (int64, bool) {
	n, ok := v.Named(path)
	switch {
	case !ok:
		return 0, false
	case n.Kind == tl.KInt32, n.Kind == tl.KInt64, n.Kind == tl.KID32, n.Kind == tl.KID64, n.Kind == tl.KFlags:
		return n.Int, true
	}
	return 0, false
}

// Str is a string or bytes field read at path: a sub-slice of the blob, as stored.
func (v view) Str(path string) ([]byte, bool) {
	n, ok := v.Named(path)
	if !ok || (n.Kind != tl.KString && n.Kind != tl.KBytes) {
		return nil, false
	}
	return n.Bytes, true
}

// ID is the constructor ID of an object read at path.
func (v view) ID(path string) (uint32, bool) {
	n, ok := v.Named(path)
	if !ok || n.Kind != tl.KObject {
		return 0, false
	}
	return n.ID, true
}

// entryAt is the object read at path (a KObject) and its table entry, looked up by the type the walker read it as and
// its ID (plan 4I4c P3, W1): never by the ID alone, since one ID can be a constructor of two types. ok is false when no
// object was read at path or tab lacks its (type, ID); id is the ID read whenever an object was read.
func entryAt(v view, tab tl.Table, path string) (id uint32, c tl.Constructor, ok bool) {
	n, found := v.Named(path)
	if !found || n.Kind != tl.KObject {
		return 0, tl.Constructor{}, false
	}
	c, ok = tab[tl.Key{Type: n.Type, ID: n.ID}]
	return n.ID, c, ok
}

// Bool is a boxed bool or a set flag-only true read at path (a flag-only true whose bit is clear is not read).
func (v view) Bool(path string) (value, ok bool) {
	n, found := v.Named(path)
	if !found || (n.Kind != tl.KBool && n.Kind != tl.KTrue) {
		return false, false
	}
	return n.Bool, true
}

// True says a bool or a flag-only true read at path holds true.
func (v view) True(path string) bool {
	b, ok := v.Bool(path)
	return ok && b
}

// stopAt is the stop of the decode when it lies at path, else nil.
func (v view) stopAt(path string) *tl.Stop {
	if s := v.r.Stop; s != nil && s.Path == path {
		return s
	}
	return nil
}

// Len is the element count of a vector read at path.
func (v view) Len(path string) (int, bool) {
	n, ok := v.Named(path)
	if !ok {
		return 0, false
	}
	switch n.Kind {
	case tl.KVector, tl.KVecInt32, tl.KVecInt64:
		return n.Len, true
	}
	return 0, false
}

// within says the stop s lies at path or inside what starts there (a nested field or a vector element).
func within(s *tl.Stop, path string) bool {
	if s == nil {
		return false
	}
	p := s.Path
	return p == path || strings.HasPrefix(p, path+".") || strings.HasPrefix(p, path+"[")
}

// inside says the decode stopped at or inside path for a reason of its own (a prefix body's end is no stop in it).
func (v view) inside(path string) bool {
	s := v.r.Stop
	return s != nil && s.Kind != tl.StopPrefix && within(s, path)
}

// Whole says the value at path was read and the stop lies neither at nor inside it (P3: read whole; a list or an object
// a stop cut is not what the wire holds).
func (v view) Whole(path string) bool { return v.Has(path) && !v.inside(path) }

// Class is the table class name of the object read at path ("" when no object was read there, or the view has no
// table).
func (v view) Class(path string) string { return className(v, v.tab, path) }
