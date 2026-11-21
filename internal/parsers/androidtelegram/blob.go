package androidtelegram

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// The blob states (spec 8.4, 4I4-P10), in vocabulary order: the note blob_states and the warning
// telegram-blob-undecoded list them in this order.
const (
	stateDecoded       = "decoded"             // every field of the top object read
	stateDecodedPrefix = "decoded-prefix"      // a prefix body read to its last pinned field (not an undecoded blob)
	statePartial       = "partial"             // a nested object of an ID the table lacks, or an opaque body
	stateUnknown       = "unknown-constructor" // the top ID is not in the table: plain columns only
	stateTruncated     = "truncated"           // the blob ends inside a field (also a length past the end, T7)
	stateMalformed     = "malformed"           // a bad bool or a bad vector constructor
	stateCap           = "cap"                 // a bound of decode/tl (vector count, depth, steps, size)
	stateBudget        = "budget"              // the host memory budget refused the decode: a fact about this run
	stateNull          = "null"                // a NULL cell
)

// blobStates are the states in vocabulary order.
func blobStates() []string {
	return []string{
		stateDecoded, stateDecodedPrefix, statePartial, stateUnknown, stateTruncated, stateMalformed, stateCap,
		stateBudget, stateNull,
	}
}

// undecodedState says a state counts as an undecoded blob (B9): every state but decoded, decoded-prefix and null.
func undecodedState(s string) bool {
	return s != stateDecoded && s != stateDecodedPrefix && s != stateNull && s != ""
}

// mappedFields are the top-level fields of a Message the record reads (spec 8.4: raw.<col>_undecided lists those
// that lie after a stop). They are the Task 5 oracle's MESSAGE_MAPPED (plan ruling T5 c1: the oracle's choice on
// this detail stands unless amended): out and id are read (direction, raw.id); entities, the blob's forwards and
// post_author are not read by any record field. A function, not the plan's package variable: a parser holds no
// mutable package state (P5, as encChatsColumns).
func mappedFields() []string {
	return []string{
		"out", "id", "from_id", "peer_id", "fwd_from", "via_bot_id", "reply_to", "date", "message", "media", "action",
		"views", "edit_date", "grouped_id", "ttl_period",
	}
}

// mappedOf is the mapped field list of a TL type (nil: no _undecided key for its blobs).
func mappedOf(typ string) []string {
	if typ == typeMessage {
		return mappedFields()
	}
	return nil
}

// stateOf maps the stop of a decode to its blob state (4I4-P10).
func stateOf(s *tl.Stop) string {
	switch {
	case s == nil:
		return stateDecoded
	case s.Kind == tl.StopUnknownConstructor && s.Path == "":
		return stateUnknown
	case s.Kind == tl.StopUnknownConstructor, s.Kind == tl.StopOpaque:
		return statePartial
	case s.Kind == tl.StopPrefix:
		return stateDecodedPrefix
	}
	return s.Kind // truncated, malformed, cap, budget: the stop kinds are the state words
}

// hexID is a constructor ID as raw states it: 0x and 8 lower-case hex digits.
func hexID(id uint32) string { return fmt.Sprintf("0x%08x", id) }

// decodeBlob decodes the non-NULL blob b of column col as the TL type typ through the table tab, charging bud
// (4I4-P13), and states it in raw (spec 8.4, the plan's state vocabulary): <col>_state, _len, _sha256, _constructor
// (when at least 4 bytes), _flags (the encoding facts, comma-joined), and after a stop other than a prefix
// _stop_kind, _stop_offset, _stop_path (below the top), _stop_id (when the stop names an ID) and _undecided. The
// caller frees the returned result's Charged() when the row's mapping returns (F8), whatever the state.
func decodeBlob(raw map[string]any, col string, b []byte, typ string, tab tl.Table, bud tl.Budget) (view, string) {
	r := tl.Decode(b, typ, tab, bud)
	v := newView(r)
	state := stateOf(r.Stop)
	raw[col+"_state"] = state
	raw[col+"_len"] = int64(len(b))
	raw[col+"_sha256"] = sha256Hex(b)
	if len(b) >= 4 {
		raw[col+"_constructor"] = hexID(binary.LittleEndian.Uint32(b))
	}
	if len(r.Flags) > 0 {
		raw[col+"_flags"] = strings.Join(r.Flags, ",")
	}
	if s := r.Stop; s != nil && s.Kind != tl.StopPrefix {
		raw[col+"_stop_kind"] = s.Kind
		raw[col+"_stop_offset"] = int64(s.Offset)
		if s.Path != "" {
			raw[col+"_stop_path"] = s.Path
		}
		if s.Kind == tl.StopUnknownConstructor || s.Kind == tl.StopOpaque {
			raw[col+"_stop_id"] = hexID(s.ID)
		}
		if und := undecided(v, tab, typ, state); len(und) > 0 {
			raw[col+"_undecided"] = strings.Join(und, ",")
		}
	}
	return v, state
}

// topOf is the top constructor of a decode when the table knows it for typ and the state is not
// unknown-constructor (a truncated, malformed or capped blob whose top ID was read has one).
func topOf(v view, tab tl.Table, typ, state string) (tl.Constructor, bool) {
	if state == stateUnknown || state == stateNull {
		return tl.Constructor{}, false
	}
	c, ok := tab[v.r.Top]
	if !ok || c.Type != typ {
		return tl.Constructor{}, false
	}
	return c, true
}

// headOf is the top-level field name of a path ("fwd_from.from_id" -> "fwd_from", "entities[0]" -> "entities").
func headOf(path string) string {
	if i := strings.IndexAny(path, ".["); i >= 0 {
		return path[:i]
	}
	return path
}

// undecided lists the mapped fields of the top constructor that start after the top-level field holding the stop
// and are present by their flags: gated by a bit that is set, by a flags word that was not read, or ungated
// (spec 8.4; the Task 5 oracle's rule). A stop at the top object itself lists nothing.
func undecided(v view, tab tl.Table, typ, state string) []string {
	mapped := mappedOf(typ)
	c, ok := topOf(v, tab, typ, state)
	if !ok || mapped == nil || v.r.Stop == nil {
		return nil
	}
	var names []string
	var fields []tl.Field
	for _, f := range c.Fields {
		if f.Kind == tl.KTrue { // a flag-only true carries no bytes: never a field that starts anywhere
			continue
		}
		names = append(names, headOf(f.Name))
		fields = append(fields, f)
	}
	head := headOf(v.r.Stop.Path)
	after := -1
	for i, n := range names {
		if n == head {
			after = i
			break
		}
	}
	if after < 0 {
		return nil
	}
	var out []string
	for i := after + 1; i < len(fields); i++ {
		name := names[i]
		if !slices.Contains(mapped, name) || slices.Contains(out, name) {
			continue
		}
		if f := fields[i]; f.Flags != "" {
			if word, read := v.Int(f.Flags); read && word>>uint(f.Bit)&1 == 0 {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}
