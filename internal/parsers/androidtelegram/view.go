package androidtelegram

import "github.com/rbenzing/minutiae/internal/decode/tl"

// view reads a decode result through the field paths of the table (`id`, `fwd_from.from_id`, `entities[0].offset`):
// at maps a path to the index of its first field in r.Fields. A path absent from the result was not read: the field
// is gated off, lies after the stop, or is not in the constructor; the caller tells these apart through the stop and
// the flags (spec 8.4), never by reading an absence as a value.
type view struct {
	r  tl.Result
	at map[string]int
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
