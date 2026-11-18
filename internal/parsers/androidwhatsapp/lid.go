package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// sourceJIDMap names the table that states a LID's phone JID.
const sourceJIDMap = "jid_map"

// lidMappings states what the database says about the LIDs among the addresses of the record (D6, spec 6.4.4): the
// thread id and the participants. A LID is mapped to a phone JID ONLY by a row of jid_map whose target jid row has a
// raw_string: raw.lid_map = [{lid, pn, source}]. The table present and no row for the LID is raw.lid_unmapped; a row
// whose target gives no phone JID is raw.lid_unresolved. A variant without the table states nothing per record (the
// note jid_map=absent says so). The participant keeps the LID as stored; the phone JID is never put in participants.
func lidMappings(m parse.MapContext, f *msgFacts, ps []message.Participant) error {
	cands := []addr{f.Chat}
	for _, p := range ps {
		if p.Address == f.Sender.Str {
			cands = append(cands, f.Sender)
		}
	}
	if badTable(m, "jid_map") {
		// the table is there but cannot be joined (no usable key or column): every LID is undecided, not unmapped
		for _, a := range cands {
			if a.JID != nil && a.JID.Class == wajid.ClassLID {
				undecidedJoin(f.Raw, "jid_map")
				break
			}
		}
		return nil
	}
	if m.Column("jid_map", "jid_row_id") < 0 {
		return nil
	}
	var mapped []any
	var unmapped, unresolved []any
	seen := map[string]bool{}
	for _, a := range cands {
		if a.Str == "" || a.JID == nil || a.JID.Class != wajid.ClassLID || seen[a.Str] {
			continue
		}
		seen[a.Str] = true
		pn, state, err := lookupLID(m, f, a)
		if err != nil {
			return err
		}
		switch state {
		case lidMapped:
			mapped = append(mapped, map[string]any{"lid": a.Str, "pn": pn, "source": sourceJIDMap})
		case lidUnmapped:
			unmapped = append(unmapped, a.Str)
		case lidUnresolved:
			unresolved = append(unresolved, a.Str)
		}
	}
	if len(mapped) > 0 {
		f.Raw["lid_map"] = mapped
	}
	if len(unmapped) > 0 {
		f.Raw["lid_unmapped"] = unmapped
	}
	if len(unresolved) > 0 {
		f.Raw["lid_unresolved"] = unresolved
	}
	return nil
}

type lidState int

const (
	lidUndecided  lidState = iota // the join cannot be decided: stated in raw.join_undecided
	lidMapped                     // jid_map names a phone JID
	lidUnmapped                   // the table has no row for the LID
	lidUnresolved                 // a row exists but its target gives no phone JID
)

// lookupLID follows a LID to its jid_map row. The jid row of the LID is the one the address was read from; a legacy
// address (stored text) reaches it through jid.raw_string, and more than one row there is undecided, never a choice.
func lookupLID(m parse.MapContext, f *msgFacts, a addr) (pn string, st lidState, err error) {
	var id int64
	switch {
	case a.RowID != nil:
		id = *a.RowID
	default:
		rows, js, err := matchText(m, "jid", "raw_string", []byte(a.Str))
		if err != nil {
			return "", lidUndecided, err
		}
		switch {
		case js != jFound:
			undecidedJoin(f.Raw, "jid")
			return "", lidUndecided, nil
		case len(rows) == 0:
			return "", lidUnmapped, nil // the LID has no jid row, so jid_map cannot name it
		case len(rows) > 1:
			undecidedJoin(f.Raw, "jid")
			return "", lidUndecided, nil
		}
		rid, ok := rows[0].Rowid()
		if !ok {
			undecidedJoin(f.Raw, "jid")
			return "", lidUndecided, nil
		}
		id = rid
	}
	mrow, js, err := getRow(m, "jid_map", id)
	if err != nil {
		return "", lidUndecided, err
	}
	switch js {
	case jNoRow:
		return "", lidUnmapped, nil
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "jid_map")
		return "", lidUndecided, nil
	}
	r := cellReader{row: mrow, cols: resolveCols(m, "jid_map", "jid_row_id"), raw: f.Raw, unknown: f.Unknown, prefix: "jid_map."}
	target, ok := r.int("jid_row_id")
	if !ok {
		return "", lidUnresolved, nil
	}
	trow, js, err := getRow(m, "jid", target)
	if err != nil {
		return "", lidUndecided, err
	}
	switch js {
	case jNoRow:
		return "", lidUnresolved, nil
	case jNoTable, jUndecided:
		undecidedJoin(f.Raw, "jid")
		return "", lidUndecided, nil
	}
	ta := addrFromJID(m, trow, f.Raw, f.Unknown, "lid_map.pn")
	if ta.Str == "" || ta.JID == nil || ta.JID.Class != wajid.ClassUser { // only a phone (user) address is a phone JID
		return "", lidUnresolved, nil
	}
	return ta.Str, lidMapped, nil
}
