package plist

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// Limits of Unarchive.
const (
	// MaxUnarchiveDepth bounds how many references deep one object is resolved.
	MaxUnarchiveDepth = 64
	// MaxUnarchiveNodes bounds the output nodes an archive may expand to, counting a shared
	// object once per reference (a consumer that walks the result pays for each visit).
	MaxUnarchiveNodes = 1 << 20
)

const (
	archiverName   = "NSKeyedArchiver"
	chargePerObjUA = 64
)

// Unarchive resolves an NSKeyedArchiver archive, given as the plain value Decode returns,
// into plain Go values: the object graph reachable from $top, with every UID reference
// replaced by the object it names. Classes become values by name: NSArray, NSSet and the
// ordered-set family a []any; NSDictionary a map[string]any (keys must resolve to strings,
// a duplicate key keeps the last and so loses an entry); NSString a string; NSData a
// []byte; NSDate a map holding the raw Cocoa seconds (an integer above 2^53 rounds to a
// float64; never a time.Time: converting a device number is decode/ts's job);
// NSURL a map of base and relative; every other class a map holding "$class" and the fields.
// $top with only "root" yields that object, otherwise a map of all its entries.
//
// A shared object is resolved once and every reference yields the same Go value (treat the
// result as read-only: a change to one reference shows at all of them); the output
// node count (shared or not) is capped at MaxUnarchiveNodes, so a small file that expands
// exponentially is ErrLimit, never expanded. A reference cycle is ErrMalformed, nesting
// deeper than MaxUnarchiveDepth is ErrLimit. The budget is charged per resolved object
// (64 + payload bytes, list slots at 16 each) and freed on any error; a nil budget is ErrNoBudget. A $archiver
// other than "NSKeyedArchiver" is ErrUnsupported; any other shape violation is ErrMalformed.
func Unarchive(v any, budget Budget) (out any, err error) {
	err = guard(func() error {
		var e error
		out, e = unarchiveCore(v, budget)
		return e
	})
	if err != nil {
		out = nil
	}
	return out, err
}

type memoEntry struct {
	val    any
	nodes  uint64
	height int // depth of the result below its own position
}

type unarchiver struct {
	objs      []any
	budget    Budget
	charged   int64
	memo      map[uint64]memoEntry
	resolving map[uint64]bool
	peak      int // deepest output position reached since ref last reset it
}

// touch records that the output reaches depth d.
func (u *unarchiver) touch(d int) { u.peak = max(u.peak, d) }

func unarchiveCore(v any, budget Budget) (any, error) {
	if budget == nil {
		return nil, fmt.Errorf("%w: no budget", ErrNoBudget)
	}
	top, objs, err := archiveParts(v)
	if err != nil {
		return nil, err
	}
	u := &unarchiver{
		objs:      objs,
		budget:    budget,
		memo:      map[uint64]memoEntry{},
		resolving: map[uint64]bool{},
	}
	ok := false
	defer func() {
		if !ok && u.charged > 0 {
			budget.Free(u.charged)
		}
	}()
	out := make(map[string]any, len(top))
	var total uint64
	for _, k := range slices.Sorted(maps.Keys(top)) {
		e := top[k]
		uid, isUID := e.(UID)
		if !isUID {
			return nil, malformed("$top entry %q is not a UID", k)
		}
		val, n, err := u.ref(uint64(uid), 0)
		if err != nil {
			return nil, err
		}
		if total = sat(total, n); total > MaxUnarchiveNodes {
			return nil, limited("archive expands beyond %d nodes", MaxUnarchiveNodes)
		}
		out[k] = val
	}
	ok = true
	if root, only := out["root"]; only && len(out) == 1 {
		return root, nil
	}
	return out, nil
}

// archiveParts checks the envelope of an archive and returns $top and $objects.
func archiveParts(v any) (top map[string]any, objs []any, err error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, malformed("an archive is a dictionary, not %T", v)
	}
	arch, ok := m["$archiver"].(string)
	if !ok {
		return nil, nil, malformed("missing $archiver")
	}
	if arch != archiverName {
		return nil, nil, fmt.Errorf("%w: archiver %q", ErrUnsupported, arch[:min(len(arch), 32)])
	}
	switch m["$version"].(type) {
	case int64, uint64, float64:
	default:
		return nil, nil, malformed("missing or non-numeric $version")
	}
	if top, ok = m["$top"].(map[string]any); !ok {
		return nil, nil, malformed("missing $top")
	}
	if objs, ok = m["$objects"].([]any); !ok {
		return nil, nil, malformed("missing $objects")
	}
	return top, objs, nil
}

func (u *unarchiver) charge(payload int) error {
	n := int64(chargePerObjUA) + int64(payload)
	if err := u.budget.Alloc(n); err != nil {
		return fmt.Errorf("%w: %w", ErrNoBudget, err)
	}
	u.charged += n
	return nil
}

// ref resolves object idx and returns its value and the output nodes it stands for.
func (u *unarchiver) ref(idx uint64, depth int) (any, uint64, error) {
	if depth > MaxUnarchiveDepth {
		return nil, 0, limited("references nested deeper than %d", MaxUnarchiveDepth)
	}
	if idx >= uint64(len(u.objs)) {
		return nil, 0, malformed("reference to object %d, the archive has %d", idx, len(u.objs))
	}
	if m, ok := u.memo[idx]; ok {
		// The result is shared, so it stands at this depth too: its height counts.
		if depth+m.height > MaxUnarchiveDepth {
			return nil, 0, limited("references nested deeper than %d", MaxUnarchiveDepth)
		}
		u.touch(depth + m.height)
		return m.val, m.nodes, nil
	}
	if u.resolving[idx] {
		return nil, 0, malformed("reference cycle through object %d", idx)
	}
	u.resolving[idx] = true
	defer delete(u.resolving, idx)
	outer := u.peak
	u.peak = depth
	val, nodes, err := u.build(idx, depth)
	if err != nil {
		return nil, 0, err
	}
	height := u.peak - depth
	u.peak = max(outer, u.peak)
	u.memo[idx] = memoEntry{val, nodes, height}
	return val, nodes, nil
}

func primitive(v any) (payload int, ok bool) {
	switch x := v.(type) {
	case string:
		return len(x), true
	case []byte:
		return len(x), true
	case RawString:
		return len(x.Bytes), true
	case int64, uint64, float64, bool, time.Time, RawDate:
		return 0, true
	}
	return 0, false
}

func (u *unarchiver) build(idx uint64, depth int) (any, uint64, error) {
	obj := u.objs[idx]
	if s, ok := obj.(string); ok && idx == 0 && s == "$null" {
		return nil, 1, nil
	}
	if n, ok := primitive(obj); ok {
		if err := u.charge(n); err != nil {
			return nil, 0, err
		}
		return obj, 1, nil
	}
	fields, ok := obj.(map[string]any)
	if !ok {
		return nil, 0, malformed("object %d is a %T, not a value or a dictionary", idx, obj)
	}
	payload := 0
	for k, e := range fields {
		payload += len(k)
		if n, ok := primitive(e); ok {
			payload += n
		} else if l, ok := e.([]any); ok {
			payload += len(l) * 16 // the slots of the output slice
		}
	}
	if err := u.charge(payload); err != nil {
		return nil, 0, err
	}
	name, err := u.className(idx, fields)
	if err != nil {
		return nil, 0, err
	}
	return u.byClass(idx, name, fields, depth)
}

// className reads the class name of the object with the given fields.
func (u *unarchiver) className(idx uint64, fields map[string]any) (string, error) {
	cu, ok := fields["$class"].(UID)
	if !ok {
		return "", malformed("object %d has no $class UID", idx)
	}
	if uint64(cu) >= uint64(len(u.objs)) {
		return "", malformed("object %d: class reference %d out of range", idx, uint64(cu))
	}
	rec, ok := u.objs[cu].(map[string]any)
	if !ok {
		return "", malformed("object %d: class record %d is not a dictionary", idx, uint64(cu))
	}
	name, ok := rec["$classname"].(string)
	if !ok {
		return "", malformed("object %d: class record %d has no $classname", idx, uint64(cu))
	}
	return name, nil
}

func (u *unarchiver) byClass(idx uint64, name string, f map[string]any, depth int) (any, uint64, error) {
	switch name {
	case "NSArray", "NSMutableArray", "NSOrderedSet", "NSMutableOrderedSet", "NSSet", "NSMutableSet":
		return u.list(idx, f, "NS.objects", depth)
	case "NSDictionary", "NSMutableDictionary":
		keys, kn, err := u.list(idx, f, "NS.keys", depth)
		if err != nil {
			return nil, 0, err
		}
		vals, vn, err := u.list(idx, f, "NS.objects", depth)
		if err != nil {
			return nil, 0, err
		}
		ks, vs := keys.([]any), vals.([]any)
		if len(ks) != len(vs) {
			return nil, 0, malformed("object %d: %d keys but %d values", idx, len(ks), len(vs))
		}
		out := make(map[string]any, len(ks))
		for i, k := range ks {
			s, ok := k.(string)
			if !ok {
				return nil, 0, fmt.Errorf("%w: object %d: dictionary key %d is not a string", ErrUnsupported, idx, i)
			}
			out[s] = vs[i]
		}
		return out, sat(1, sat(kn, vn)), nil
	case "NSString", "NSMutableString":
		v, n, err := u.field(f, "NS.string", depth)
		if err != nil {
			return nil, 0, err
		}
		switch v.(type) {
		case string, RawString:
			return v, n, nil
		}
		return nil, 0, malformed("object %d: NS.string is not a string", idx)
	case "NSData", "NSMutableData":
		v, n, err := u.field(f, "NS.data", depth)
		if err != nil {
			return nil, 0, err
		}
		if b, ok := v.([]byte); ok {
			return b, n, nil
		}
		return nil, 0, malformed("object %d: NS.data is not data", idx)
	case "NSDate":
		v, n, err := u.field(f, "NS.time", depth)
		if err != nil {
			return nil, 0, err
		}
		var secs float64
		switch x := v.(type) {
		case float64:
			secs = x
		case int64:
			secs = float64(x)
		default:
			return nil, 0, malformed("object %d: NS.time is not a number", idx)
		}
		return map[string]any{"$class": "NSDate", "NS.time": secs}, sat(1, n), nil
	case "NSURL":
		base, bn, err := u.field(f, "NS.base", depth)
		if err != nil {
			return nil, 0, err
		}
		rel, rn, err := u.field(f, "NS.relative", depth)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"$class": "NSURL", "NS.base": base, "NS.relative": rel}, sat(1, sat(bn, rn)), nil
	}
	out := map[string]any{"$class": name}
	nodes := uint64(1)
	for _, k := range slices.Sorted(maps.Keys(f)) {
		e := f[k]
		if k == "$class" {
			continue
		}
		v, n, err := u.value(e, depth)
		if err != nil {
			return nil, 0, err
		}
		if nodes = sat(nodes, n); nodes > MaxUnarchiveNodes {
			return nil, 0, limited("archive expands beyond %d nodes", MaxUnarchiveNodes)
		}
		out[k] = v
	}
	return out, nodes, nil
}

// field resolves the named field of an object; a missing one is nil.
func (u *unarchiver) field(f map[string]any, key string, depth int) (any, uint64, error) {
	e, ok := f[key]
	if !ok {
		return nil, 1, nil
	}
	return u.value(e, depth)
}

// list resolves the named field, which must be an array, into a []any.
func (u *unarchiver) list(idx uint64, f map[string]any, key string, depth int) (any, uint64, error) {
	raw, ok := f[key].([]any)
	if !ok {
		return nil, 0, malformed("object %d: %s is missing or not an array", idx, key)
	}
	out := make([]any, len(raw))
	nodes := uint64(1)
	for i, e := range raw {
		v, n, err := u.value(e, depth)
		if err != nil {
			return nil, 0, err
		}
		if nodes = sat(nodes, n); nodes > MaxUnarchiveNodes {
			return nil, 0, limited("archive expands beyond %d nodes", MaxUnarchiveNodes)
		}
		out[i] = v
	}
	return out, nodes, nil
}

// value resolves a field value: a UID is followed, containers are resolved inside, a
// primitive is itself.
func (u *unarchiver) value(e any, depth int) (any, uint64, error) {
	switch x := e.(type) {
	case UID:
		return u.ref(uint64(x), depth+1)
	case []any:
		if depth+1 > MaxUnarchiveDepth {
			return nil, 0, limited("nested deeper than %d", MaxUnarchiveDepth)
		}
		u.touch(depth + 1)
		out := make([]any, len(x))
		nodes := uint64(1)
		for i, el := range x {
			v, n, err := u.value(el, depth+1)
			if err != nil {
				return nil, 0, err
			}
			if nodes = sat(nodes, n); nodes > MaxUnarchiveNodes {
				return nil, 0, limited("archive expands beyond %d nodes", MaxUnarchiveNodes)
			}
			out[i] = v
		}
		return out, nodes, nil
	case map[string]any:
		if depth+1 > MaxUnarchiveDepth {
			return nil, 0, limited("nested deeper than %d", MaxUnarchiveDepth)
		}
		u.touch(depth + 1)
		out := make(map[string]any, len(x))
		nodes := uint64(1)
		for _, k := range slices.Sorted(maps.Keys(x)) {
			el := x[k]
			v, n, err := u.value(el, depth+1)
			if err != nil {
				return nil, 0, err
			}
			if nodes = sat(nodes, n); nodes > MaxUnarchiveNodes {
				return nil, 0, limited("archive expands beyond %d nodes", MaxUnarchiveNodes)
			}
			out[k] = v
		}
		return out, nodes, nil
	}
	if _, ok := primitive(e); ok {
		return e, 1, nil
	}
	return nil, 0, malformed("unsupported value of type %T", e)
}
