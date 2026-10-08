package common

// CopyMap deep-copies a free-form object (maps and []any), so a payload built
// from it never aliases the typed value it came from. Copying stops at the depth
// the validator refuses anyway (deeper values become nil), so a hostile nesting
// costs bounded work. A nil map copies to an empty one.
func CopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = copyValue(v, 0)
	}
	return out
}

func copyValue(v any, depth int) any {
	if depth > MaxContainerDepth {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = copyValue(e, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyValue(e, depth+1)
		}
		return out
	}
	return v
}

// NormMap deep-copies a free-form object read with json.Decoder.UseNumber. Numbers
// stay json.Number, exactly as stored (an integer beyond int64 or a long decimal is
// never rounded), so a typed value rebuilt from a stored payload equals it. Values deeper
// than the validator allows become nil. A nil map becomes an empty one.
func NormMap(in map[string]any) map[string]any { return normMap(in, 0) }

func normMap(in map[string]any, depth int) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normValue(v, depth+1)
	}
	return out
}

func normValue(v any, depth int) any {
	if depth > MaxContainerDepth {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		return normMap(x, depth)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normValue(e, depth+1)
		}
		return out
	}
	return v
}

// IsLive reports whether a typed record is a plain live one: it carries none of
// the provenance objects recovery, snapshot and deleted. Presence is what counts,
// so an EMPTY object (a stored "recovery": {}) still makes the record not live.
// Every typed reader (the Live methods of the record types) uses this one
// function, so no reader re-implements the rule.
func IsLive(recovery, snapshot, deleted map[string]any) bool {
	return recovery == nil && snapshot == nil && deleted == nil
}
