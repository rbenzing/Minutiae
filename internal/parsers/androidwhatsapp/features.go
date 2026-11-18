package androidwhatsapp

import (
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The WhatsApp message features of spec 6.5. Every join is by message_row_id, the rowid alias of its table (Probe
// checks it); a join that cannot be decided is listed in raw.join_undecided, never read as "no such row".

// revokeFacts says why a message is a tombstone ("deleted for everyone"): the app's own structure, a message_revoked row
// (Source "message_revoked") or only the type (Source "message_type").
type revokeFacts struct{ Source string }

const (
	sourceRevokedRow  = "message_revoked"
	sourceRevokedType = "message_type"
)

// sideRow returns the row of a message_row_id table for the message, nil when the variant lacks the table or the
// message has no row. A join that cannot be decided is stated.
func sideRow(m parse.MapContext, f *msgFacts, table string) (parse.Row, error) {
	if m.Column(table, "message_row_id") < 0 {
		return nil, nil
	}
	row, st, err := getRow(m, table, f.Rowid)
	if err != nil {
		return nil, err
	}
	switch st {
	case jFound:
		return row, nil
	case jUndecided, jNoTable:
		undecidedJoin(f.Raw, table)
	}
	return nil, nil
}

// sideReader reads the cells of a side-table row; unknown cells are listed under "<table>.<column>".
func sideReader(m parse.MapContext, f *msgFacts, row parse.Row, table string, cols ...string) cellReader {
	return cellReader{row: row, cols: rowrun.Resolve(m, table, cols...), raw: f.Raw, unknown: f.Unknown, prefix: table + "."}
}

// num reads a REAL cell (an INTEGER one is accepted: a column of REAL affinity may hold it): a finite number, or the
// marker "non-finite".
func (r cellReader) num(name string) (any, bool) {
	col := r.cols.At(name)
	if col < 0 || r.row.IsNull(col) {
		return nil, false
	}
	if v, ok := r.row.Float(col); ok {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "non-finite", true
		}
		return v, true
	}
	if v, ok := r.row.Int(col); ok {
		return v, true
	}
	r.unknown[r.prefix+name] = "type-mismatch"
	return nil, false
}

// jidString reads the JID of a jid row key (> 0) for a feature: its string, or the state that says why there is none.
func jidString(m parse.MapContext, f *msgFacts, id int64, field string) (a addr, st string, err error) {
	row, js, err := getRow(m, "jid", id)
	if err != nil {
		return addr{}, "", err
	}
	switch js {
	case jNoRow:
		return addr{}, stateNoJIDRow, nil
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "jid")
		return addr{}, stateUndecided, nil
	}
	a = addrFromJID(m, row, f.Raw, f.Unknown, field)
	return a, a.State, nil
}

// readModernFeatures reads the side tables of a modern message row.
func readModernFeatures(m parse.MapContext, f *msgFacts) error {
	label, _ := f.Type.label()
	// revoke: a message_revoked row, or a type that says so (15, 64)
	rrow, err := sideRow(m, f, "message_revoked")
	if err != nil {
		return err
	}
	switch {
	case rrow != nil:
		f.Revoke = &revokeFacts{Source: sourceRevokedRow}
		if err := readRevoke(m, f, rrow); err != nil {
			return err
		}
	case label == labelRevoked:
		f.Revoke = &revokeFacts{Source: sourceRevokedType}
	}
	// quote: the denormalised copy of the message that was replied to (never a record, never a body)
	if row, err := sideRow(m, f, "message_quoted"); err != nil {
		return err
	} else if row != nil {
		if err := readQuoted(m, f, row); err != nil {
			return err
		}
	}
	// edit
	if row, err := sideRow(m, f, "message_edit_info"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_edit_info", "original_key_id", "edited_timestamp", "sender_timestamp", "display_type")
		edit := map[string]any{}
		if b, ok := r.text("original_key_id"); ok {
			edit["original_key_id"] = cleanField(f.Raw, "edit.original_key_id", b)
		}
		for _, n := range []string{"edited_timestamp", "sender_timestamp"} {
			if v, ok := r.int(n); ok {
				edit[n], edit[n+"_unit"] = v, unitUnestablished
			}
		}
		if v, ok := r.int("display_type"); ok {
			edit["display_type"] = v
		}
		f.Raw["edit"] = edit
	}
	// a system row, a call notice, a location
	if label == labelSystem {
		if err := readSystem(m, f); err != nil {
			return err
		}
	}
	if f.Type.OK && f.Type.Value == typeCallNotice {
		if row, err := sideRow(m, f, "message_call_log"); err != nil {
			return err
		} else if row != nil {
			if v, ok := sideReader(m, f, row, "message_call_log", "call_log_row_id").int("call_log_row_id"); ok {
				f.Raw["call_log_row_id"] = v
			}
		}
	}
	if label == "location" || label == "live_location" {
		if row, err := sideRow(m, f, "message_location"); err != nil {
			return err
		} else if row != nil {
			readLocation(m, f, row)
		}
	}
	return readExtras(m, f)
}

// readRevoke reads the message_revoked row: raw.revoke = {revoked_key_id, admin_jid, revoke_timestamp}. The time is kept
// raw: its unit is not established (R8).
func readRevoke(m parse.MapContext, f *msgFacts, row parse.Row) error {
	r := sideReader(m, f, row, "message_revoked", "revoked_key_id", "admin_jid_row_id", "revoke_timestamp")
	rv := map[string]any{}
	if b, ok := r.text("revoked_key_id"); ok {
		rv["revoked_key_id"] = cleanField(f.Raw, "revoke.revoked_key_id", b)
	}
	if id, ok := r.int("admin_jid_row_id"); ok && id > 0 {
		a, st, err := jidString(m, f, id, "revoke.admin_jid")
		if err != nil {
			return err
		}
		switch {
		case a.Str != "":
			rv["admin_jid"] = a.Str
		case st != "":
			rv["admin_jid_state"] = st
			if a.Parts != nil {
				rv["admin_jid_parts"] = a.Parts
			}
		}
	}
	if v, ok := r.int("revoke_timestamp"); ok {
		rv["revoke_timestamp"], rv["revoke_timestamp_unit"] = v, unitUnestablished
	}
	f.Raw["revoke"] = rv
	return nil
}

// readQuoted reads a message_quoted row: raw.quoted = {key_id, from_me, sender_jid, message_type, timestamp, text, copy}.
func readQuoted(m parse.MapContext, f *msgFacts, row parse.Row) error {
	r := sideReader(m, f, row, "message_quoted", "key_id", "from_me", "sender_jid_row_id", "message_type", "timestamp", "text_data")
	q := map[string]any{"copy": true}
	if b, ok := r.text("key_id"); ok {
		q["key_id"] = cleanField(f.Raw, "quoted.key_id", b)
	}
	for _, n := range []string{"from_me", "message_type", "timestamp"} {
		if v, ok := r.int(n); ok {
			q[n] = v
		}
	}
	if id, ok := r.int("sender_jid_row_id"); ok && id > 0 {
		a, st, err := jidString(m, f, id, "quoted.sender_jid")
		if err != nil {
			return err
		}
		switch {
		case a.Str != "":
			q["sender_jid"] = a.Str
		case st != "":
			q["sender_jid_state"] = st
		}
	}
	if b, ok := r.text("text_data"); ok && len(b) > 0 {
		q["text"] = cleanField(f.Raw, "quoted.text", b)
	}
	f.Raw["quoted"] = q
	return nil
}

// readSystem reads the system tables of a system row: raw.action_type with its label (only 18 is mapped, 4I-R18),
// raw.old_data and raw.number_change.
func readSystem(m parse.MapContext, f *msgFacts) error {
	if row, err := sideRow(m, f, "message_system"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_system", "action_type")
		if v, ok := r.int("action_type"); ok {
			f.Raw["action_type"] = v
			if l, ok := lookupEnum(colActionType, v); ok {
				f.Raw["label_action_type"] = l
			}
		}
	}
	if row, err := sideRow(m, f, "message_system_value_change"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_system_value_change", "old_data")
		if b, ok := r.text("old_data"); ok {
			f.Raw["old_data"] = cleanField(f.Raw, "old_data", b)
		}
	}
	if row, err := sideRow(m, f, "message_system_number_change"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_system_number_change", "old_jid_row_id", "new_jid_row_id")
		nc := map[string]any{}
		for _, n := range []string{"old_jid_row_id", "new_jid_row_id"} {
			if v, ok := r.int(n); ok {
				nc[n] = v
			}
		}
		f.Raw["number_change"] = nc
	}
	return nil
}

// readLocation keeps the columns of a message_location row as stored (spec 6.5).
func readLocation(m parse.MapContext, f *msgFacts, row parse.Row) {
	r := sideReader(m, f, row, "message_location", "latitude", "longitude", "place_name", "place_address", "url",
		"live_location_share_duration", "live_location_sequence_number", "live_location_final_latitude",
		"live_location_final_longitude", "live_location_final_timestamp", "map_download_status")
	loc := map[string]any{}
	for _, n := range []string{"latitude", "longitude", "live_location_final_latitude", "live_location_final_longitude"} {
		if v, ok := r.num(n); ok {
			loc[n] = v
		}
	}
	for _, n := range []string{"place_name", "place_address", "url"} {
		if b, ok := r.text(n); ok {
			loc[n] = cleanField(f.Raw, "location."+n, b)
		}
	}
	for _, n := range []string{"live_location_share_duration", "live_location_sequence_number", "live_location_final_timestamp", "map_download_status"} {
		if v, ok := r.int(n); ok {
			loc[n] = v
		}
	}
	if len(loc) > 0 {
		f.Raw["location"] = loc
	}
}

// readExtras keeps the single-source features as stored: view-once state, ephemeral settings, forwarding. Their meaning
// is not established, and the expiry time's unit is not either (R8).
func readExtras(m parse.MapContext, f *msgFacts) error {
	if row, err := sideRow(m, f, "message_view_once_media"); err != nil {
		return err
	} else if row != nil {
		if v, ok := sideReader(m, f, row, "message_view_once_media", "state").int("state"); ok {
			f.Raw["view_once_state"] = v
		}
	}
	if row, err := sideRow(m, f, "message_ephemeral"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_ephemeral", "duration", "expire_timestamp")
		e := map[string]any{}
		if v, ok := r.int("duration"); ok {
			e["duration"] = v
		}
		if v, ok := r.int("expire_timestamp"); ok {
			e["expire_timestamp"], e["expire_timestamp_unit"] = v, unitUnestablished
		}
		f.Raw["ephemeral"] = e
	}
	if row, err := sideRow(m, f, "message_forwarded"); err != nil {
		return err
	} else if row != nil {
		r := sideReader(m, f, row, "message_forwarded", "forward_score", "forward_origin")
		fw := map[string]any{}
		for _, n := range []string{"forward_score", "forward_origin"} {
			if v, ok := r.int(n); ok {
				fw[n] = v
			}
		}
		f.Raw["forwarded"] = fw
	}
	return nil
}

// readLegacyFeatures reads what a legacy row has: the quote of messages_quotes through quoted_row_id, a location in its
// own columns, and, in a transition store, the side tables keyed to messages._id (S2 triggers, research 3.3): a revoke,
// the system tables of a system row, and the extras.
func readLegacyFeatures(m parse.MapContext, r cellReader, f *msgFacts) error {
	if qid, ok := r.int("quoted_row_id"); ok {
		f.Raw["quoted_row_id"] = qid
		if qid > 0 {
			if err := readLegacyQuote(m, f, qid); err != nil {
				return err
			}
		}
	}
	if l, ok := f.Type.label(); ok && l == "location" && !f.LegacySys {
		loc := map[string]any{}
		for _, n := range []string{"latitude", "longitude"} {
			if v, ok := r.num(n); ok {
				loc[n] = v
			}
		}
		if len(loc) > 0 {
			f.Raw["location"] = loc
		}
	}
	if row, err := sideRow(m, f, "message_revoked"); err != nil {
		return err
	} else if row != nil {
		f.Revoke = &revokeFacts{Source: sourceRevokedRow}
		if err := readRevoke(m, f, row); err != nil {
			return err
		}
	}
	if f.LegacySys {
		if err := readSystem(m, f); err != nil {
			return err
		}
	}
	return readExtras(m, f)
}

// readLegacyQuote reads a messages_quotes row (a copy of the quoted message in the layout of messages).
func readLegacyQuote(m parse.MapContext, f *msgFacts, id int64) error {
	row, st, err := getRow(m, "messages_quotes", id)
	if err != nil {
		return err
	}
	switch st {
	case jNoRow:
		f.Raw["quoted_state"] = "no-messages_quotes-row"
		return nil
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "messages_quotes")
		return nil
	}
	r := cellReader{
		row: row, cols: rowrun.Resolve(m, "messages_quotes", "key_id", "key_from_me", "remote_resource", "media_wa_type", "timestamp", "data"),
		raw: f.Raw, unknown: f.Unknown, prefix: "messages_quotes.",
	}
	q := map[string]any{"copy": true}
	if b, ok := r.text("key_id"); ok {
		q["key_id"] = cleanField(f.Raw, "quoted.key_id", b)
	}
	if v, ok := r.int("key_from_me"); ok {
		q["from_me"] = v
	}
	if v, ok := r.int("timestamp"); ok {
		q["timestamp"] = v
	}
	if t := r.stored("media_wa_type"); t != nil {
		q["message_type"] = t
	}
	if b, ok := r.text("remote_resource"); ok && len(b) > 0 {
		q["sender_jid"] = cleanField(f.Raw, "quoted.sender_jid", b)
	}
	if b, ok := r.text("data"); ok && len(b) > 0 {
		q["text"] = cleanField(f.Raw, "quoted.text", b)
	}
	f.Raw["quoted"] = q
	return nil
}
