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
// this detail stands unless amended): out and id are read (direction, raw.id); plan 4I4b adds the fourteen tail
// fields after ttl_period (tailRaw), which no record field reads. A function, not the plan's package variable: a parser holds no
// mutable package state (P5, as encChatsColumns).
func mappedFields() []string {
	return []string{
		"out", "id", "from_id", "peer_id", "fwd_from", "via_bot_id", "reply_to", "date", "message", "media", "action",
		"views", "edit_date", "grouped_id", "ttl_period",
		"forwards", "post_author", "replies", "reactions", "restriction_reason", "quick_reply_shortcut_id", "effect", "factcheck",
		"report_delivery_until_date", "paid_message_stars", "suggested_post", "schedule_repeat_period", "summary_from_language",
		"entities",
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

// The TL columns a mapping decodes: the row's own blob, whose undecided fields are listed (spec 8.4), and the reply
// copy (spec 8.5).
const (
	colData      = "data"
	colReplyData = "replydata"
)

// hexID is a constructor ID as raw states it: 0x and 8 lower-case hex digits.
func hexID(id uint32) string { return fmt.Sprintf("0x%08x", id) }

// decodeBlob decodes the non-NULL blob b of column col as the TL type typ through the table tab, charging bud
// (4I4-P13), and states it in raw (spec 8.4, the plan's state vocabulary): <col>_state, _len, _sha256, _constructor
// (when at least 4 bytes), _flags (the encoding facts, comma-joined), and after a stop other than a prefix
// _stop_kind, _stop_offset, _stop_path (below the top), _stop_id (when the stop names an ID) and, for the row's own
// blob (col "data"), _undecided. The caller frees the returned result's Charged() when the row's mapping returns
// (F8), whatever the state.
func decodeBlob(raw map[string]any, col string, b []byte, typ string, tab tl.Table, bud tl.Budget) (view, string) {
	r := tl.Decode(b, typ, tab, bud)
	v := newView(r)
	v.tab = tab
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
		if col == colData { // a side blob (replydata, a joined chats.data) lists nothing: its fields are not the record's
			if und := undecided(v, tab, typ, state); len(und) > 0 {
				raw[col+"_undecided"] = strings.Join(und, ",")
			}
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

// topHeads are the top-level field names of c's fields that carry bytes (a flag-only true carries none: never a
// field that starts anywhere), in order, with the fields themselves.
func topHeads(c tl.Constructor) ([]string, []tl.Field) {
	var names []string
	var fields []tl.Field
	for _, f := range c.Fields {
		if f.Kind == tl.KTrue {
			continue
		}
		names = append(names, headOf(f.Name))
		fields = append(fields, f)
	}
	return names, fields
}

// lastIndex is the index of the last name equal to s, -1 when none (a head may span several fields, such as an old
// layer's fwd_from.from_id and fwd_from.date: the head ends with its last field; review M3).
func lastIndex(names []string, s string) int {
	for i := len(names) - 1; i >= 0; i-- {
		if names[i] == s {
			return i
		}
	}
	return -1
}

// stopAfter says the decode stopped at a top-level field after field name of the top constructor c: the fields up to
// and including name were all read (or gated off by a flags word that was read).
func stopAfter(v view, c tl.Constructor, name string) bool {
	s := v.r.Stop
	if s == nil {
		return false
	}
	names, _ := topHeads(c)
	at, target := lastIndex(names, headOf(s.Path)), lastIndex(names, name)
	return at >= 0 && target >= 0 && at > target
}

// ownReadFailed says the stop is the failed read of a top-level field itself (a truncated, malformed, capped or
// refused int, string, bool, flags word or object ID), not of something nested in it: the field's value was not read.
func ownReadFailed(s *tl.Stop) bool {
	switch s.Kind {
	case tl.StopTruncated, tl.StopMalformed, tl.StopCap, tl.StopBudget:
		return s.Path != "" && s.Path == headOf(s.Path)
	}
	return false
}

// undecided lists the mapped fields of the top constructor that are not read because of the stop (spec 8.4, 8.5):
// the stopped field itself when the stop is its own read (review M2), then the fields that start after the last
// field of the stop's head (review M3) and are present by their flags: gated by a bit that is set, by a flags word
// that was not read, or ungated. A stop at the top object itself lists nothing.
func undecided(v view, tab tl.Table, typ, state string) []string {
	mapped := mappedOf(typ)
	c, ok := topOf(v, tab, typ, state)
	if !ok || mapped == nil || v.r.Stop == nil {
		return nil
	}
	names, fields := topHeads(c)
	head := headOf(v.r.Stop.Path)
	after := lastIndex(names, head)
	if after < 0 {
		return nil
	}
	var out []string
	if ownReadFailed(v.r.Stop) && slices.Contains(mapped, head) {
		out = append(out, head)
	}
	for i := after + 1; i < len(fields); i++ {
		name := names[i]
		if name == head || !slices.Contains(mapped, name) || slices.Contains(out, name) {
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
