package androidgooglemessages

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// The names of the states the joins state (spec 7; the plan's "State vocabularies").
const (
	stateUndecided     = "undecided"
	stateNull          = "null"
	stateAbsent        = "absent"
	stateNoParticipant = "no-participants-row"
	stateNoConv        = "no-conversations-row"
	stateSelfUndecided = "self-undecided"
	stateSelf          = "self"
)

// maxName bounds a free-text name (a full name, a conversation name, a subscription name); a name is cleaned and may be
// cut, flagged in raw.text_flags, because it is not an identifier.
const maxName = 1 << 10

// otherThanSelfSubID is the sub_id of every participant that is not the device owner: a participant is the self when its
// sub_id differs (facts.tsv self.is_self, ParticipantData.java:505-507@M; held equal by TestGoogleMessagesSelfRuleMatchesFacts).
// contactUnresolved is participants.contact_id of a participant with no contacts link (facts.tsv contact_id.unresolved,
// ParticipantData.java:60@M).
const (
	otherThanSelfSubID = -2
	contactUnresolved  = -1
)

// selfness says whether a participants row is the device owner: the cited rule sub_id != -2 (B2, R7). A sub_id that is NULL
// or cannot be read is undecided, never "not self".
type selfness uint8

const (
	selfNo selfness = iota
	selfYes
	selfUndecided
)

// selfOf applies the cited self rule to the participants row (col is its sub_id column). An unreadable cell is listed in
// unknown as participants.sub_id.
func selfOf(prow parse.Row, col int, unknown map[string]any) selfness {
	v, k := intCellOf(prow, col)
	if k == rowrun.CellValue {
		if v == otherThanSelfSubID {
			return selfNo
		}
		return selfYes
	}
	rowrun.Flag(unknown, prow, "participants.sub_id", col, k)
	return selfUndecided
}

// party is one participants row as the mappings use it: a non-self participant with a usable address (a member or a
// sender). Name is the cached contact name, set only with a resolved contact_id (spec 6.3).
type party struct {
	ID            int64
	Address, Kind string
	Name          string
	Self          selfness
	Blocked       bool
}

// participantColumns are the columns of participants the mappings read (spec 6.3).
func participantColumns() []string {
	return []string{
		"sub_id", "sim_slot_id", "normalized_destination", "send_destination", "display_destination", "full_name", "contact_id",
		"blocked", "subscription_name",
	}
}

// undecidedJoin lists a join in raw.join_undecided (sorted, once; restated from androidtelegram, F29).
func undecidedJoin(raw map[string]any, table string) {
	cur, _ := raw["join_undecided"].([]any)
	if slices.Contains(cur, any(table)) {
		return
	}
	cur = append(cur, table)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["join_undecided"] = cur
}

// joinUndecided says an error of a join is the join being undecided (a table the database lacks or cannot read, a guarded
// table, an undecidable key), not a budget, context or I/O failure, which fails the job.
func joinUndecided(err error) bool {
	return errors.Is(err, sqlitedb.ErrNoSuchTable) || dataDamage(err)
}

// participantRow is the rowid-alias join to participants (spec 6.3): the row, or the state of a miss (no-participants-row,
// or undecided and the table named in raw.join_undecided when the join cannot be trusted). Never a silent absence.
func participantRow(m parse.MapContext, id int64, raw map[string]any) (parse.Row, string, error) {
	row, found, err := m.Get(tableParticipants, id)
	switch {
	case err != nil && joinUndecided(err):
		undecidedJoin(raw, tableParticipants)
		return nil, stateUndecided, nil
	case err != nil:
		return nil, "", err
	case !found:
		return nil, stateNoParticipant, nil
	}
	return row, "", nil
}

// partyFrom is the party of a non-self participants row, or false with the reason in raw (R9): the address is
// send_destination when non-empty, else normalized_destination, else display_destination (raw.<prefix>_address_basis names
// it); empty, altered by cleaning or over 1 KiB (as stored or once cleaned) it is not an address and
// raw.<prefix>_address_state says why. The name is full_name only with a resolved contact_id (raw.<prefix>_name_basis).
// A prefix "" states nothing in raw (a member: its parts are summed in members_*).
func partyFrom(c rowrun.Cols, prow parse.Row, raw, unknown map[string]any, prefix string) (party, bool) {
	dests := map[string][]byte{}
	for _, col := range []string{"normalized_destination", "send_destination", "display_destination"} {
		b, k := textCellOf(prow, c.At(col))
		rowrun.Flag(unknown, prow, "participants."+col, c.At(col), k)
		if k == rowrun.CellValue {
			dests[col] = b
		}
	}
	if prefix != "" {
		shown := map[string]any{}
		for col, b := range dests {
			if len(b) > maxIdent {
				continue
			}
			if cl := common.CleanText(b, 3*maxIdent); len(cl.Text) <= maxIdent { // an identifier is never cut
				if len(cl.Flags) > 0 {
					markText(raw, prefix+"_destinations")
				}
				shown[col] = cl.Text
			}
		}
		if len(shown) > 0 {
			raw[prefix+"_destinations"] = shown
		}
	}
	chosen := ""
	for _, col := range []string{"send_destination", "normalized_destination", "display_destination"} {
		if len(dests[col]) > 0 {
			chosen = col
			break
		}
	}
	if chosen == "" {
		if prefix != "" {
			raw[prefix+"_address_state"] = "empty"
		}
		return party{}, false
	}
	b := dests[chosen]
	cl := common.CleanText(b, 3*maxIdent)
	if len(b) > maxIdent || len(cl.Text) > maxIdent {
		if prefix != "" {
			raw[prefix+"_address_state"] = "too-long"
			raw[prefix+"_address_len"] = int64(len(b))
			raw[prefix+"_address_sha256"] = sha256Hex(b)
		}
		return party{}, false
	}
	if len(cl.Flags) > 0 {
		if prefix != "" {
			raw[prefix+"_address_state"] = "altered"
			raw[prefix+"_address_text"] = cl.Text
		}
		return party{}, false
	}
	if prefix != "" {
		raw[prefix+"_address_basis"] = chosen
	}
	p := party{Address: cl.Text, Kind: common.AddressKind(cl.Text)}
	nameRaw := raw
	if prefix == "" {
		nameRaw = map[string]any{} // a member's flags are not stated per record
	}
	fb, fk := textCellOf(prow, c.At("full_name"))
	rowrun.Flag(unknown, prow, "participants.full_name", c.At("full_name"), fk)
	cid, ck := intCellOf(prow, c.At("contact_id"))
	rowrun.Flag(unknown, prow, "participants.contact_id", c.At("contact_id"), ck)
	if fk == rowrun.CellValue {
		key := prefix + "_full_name"
		if prefix == "" {
			key = "member_full_name"
		}
		name := cleanField(nameRaw, key, fb, maxName)
		if prefix != "" && name != "" {
			raw[prefix+"_full_name"] = name
		}
		if ck == rowrun.CellValue && cid != contactUnresolved && name != "" {
			p.Name = name
			if prefix != "" {
				raw[prefix+"_name_basis"] = "cached_contact"
			}
		}
	}
	if prefix != "" && ck == rowrun.CellValue {
		raw[prefix+"_contact_id"] = cid
	}
	return p, true
}

// partiesResult is what the joins of a message say (spec 6.3).
type partiesResult struct {
	Participants []message.Participant
	Unknown      bool
	Thread       *message.Thread
	Subscription *int64
	Direction    string // after the cross-check (B7)
	Counterparty string
}

// partiesOf joins the sender, the self row and the conversation of a messages row (spec 6.2 cross-check, 6.3, 4I5-P3). It
// states every join in raw: sender_id, sender_is_self, sender_state, direction_conflict, self_id and its row, the
// conversation and its members. e must not be nil (the typed pass joins nothing).
func partiesOf(m parse.MapContext, e *env, row parse.Row, c rowrun.Cols, raw, unknown map[string]any, direction string) (partiesResult, error) {
	res := partiesResult{Direction: direction}
	if e.Guard != nil {
		m = guardedCtx{MapContext: m, g: e.Guard}
	}
	pc := rowrun.Resolve(m, tableParticipants, participantColumns()...)

	// the sender, and the direction cross-check (B7)
	var sender parse.Row
	senderSelf := selfUndecided
	sid, sk := intCellOf(row, c.At("sender_id"))
	rowrun.Flag(unknown, row, "sender_id", c.At("sender_id"), sk)
	switch sk {
	case rowrun.CellValue:
		raw["sender_id"] = sid
		prow, state, err := participantRow(m, sid, raw)
		switch {
		case err != nil:
			return res, err
		case prow == nil:
			raw["sender_state"] = state
		default:
			switch sx := selfOf(prow, pc.At("sub_id"), unknown); sx {
			case selfUndecided:
				raw["sender_state"] = stateSelfUndecided
			default:
				sender, senderSelf = prow, sx
				raw["sender_is_self"] = sx == selfYes
			}
		}
	case rowrun.CellNull:
		raw["sender_state"] = stateAbsent
	case rowrun.CellMissing:
	default:
		raw["sender_state"] = stateUndecided
	}
	if direction != "unknown" && senderSelf != selfUndecided && (direction == "in") == (senderSelf == selfYes) {
		raw["direction_conflict"] = map[string]any{"status_direction": direction, "sender_is_self": senderSelf == selfYes}
		res.Direction = "unknown"
	}

	// the self row (the SIM the message belongs to)
	if err := selfRowOf(m, row, c, pc, raw, unknown, &res); err != nil {
		return res, err
	}

	// the conversation, its members
	th, members, err := threadOf(m, e, row, c, pc, raw, unknown)
	if err != nil {
		return res, err
	}
	res.Thread = th

	// the parties (4I5-P3): the current members of an outgoing message (C9), the non-self sender of any other
	switch {
	case res.Direction == "out":
		if len(members) > 0 {
			for _, p := range members {
				res.Participants = append(res.Participants, message.Participant{Role: "member", Address: p.Address, Name: p.Name, Kind: p.Kind})
			}
			raw["members_basis"] = "current"
			res.Counterparty = members[0].Address
		}
	case sender != nil && senderSelf == selfNo:
		if p, ok := partyFrom(pc, sender, raw, unknown, "from"); ok {
			res.Participants = []message.Participant{{Role: "from", Address: p.Address, Name: p.Name, Kind: p.Kind}}
			res.Counterparty = p.Address
		}
	case senderSelf == selfYes:
		raw["sender_state"] = stateSelf
	}
	res.Unknown = len(res.Participants) == 0
	return res, nil
}

// selfRowOf states the self_id join (4I5-P5): raw.self_id and, from the row, self_sub_id, self_sim_slot_id and
// self_subscription_name; the subscription of the record is the sub_id when it is >= 0 (the android.mmssms rule).
func selfRowOf(m parse.MapContext, row parse.Row, c rowrun.Cols, pc rowrun.Cols, raw, unknown map[string]any, res *partiesResult) error {
	fid, fk := intCellOf(row, c.At("self_id"))
	rowrun.Flag(unknown, row, "self_id", c.At("self_id"), fk)
	switch fk {
	case rowrun.CellValue:
		raw["self_id"] = fid
		srow, state, err := participantRow(m, fid, raw)
		switch {
		case err != nil:
			return err
		case srow == nil:
			raw["self_row_state"] = state
			return nil
		}
		for _, col := range []string{"sub_id", "sim_slot_id"} {
			x, xk := intCellOf(srow, pc.At(col))
			rowrun.Flag(unknown, srow, "participants."+col, pc.At(col), xk)
			if xk != rowrun.CellValue {
				continue
			}
			raw["self_"+col] = x
			if col == "sub_id" && x >= 0 {
				res.Subscription = &x
			}
		}
		if nb, nk := textCellOf(srow, pc.At("subscription_name")); nk == rowrun.CellValue {
			raw["self_subscription_name"] = cleanField(raw, "self_subscription_name", nb, maxName)
		} else {
			rowrun.Flag(unknown, srow, "participants.subscription_name", pc.At("subscription_name"), nk)
		}
	case rowrun.CellNull:
		raw["self_row_state"] = stateAbsent
	case rowrun.CellMissing:
	default:
		raw["self_row_state"] = stateUndecided
	}
	return nil
}

// conversationIntKeys are the stored integers of the conversation kept in raw under these names (spec 6.3).
var conversationIntKeys = [][2]string{
	{"sms_thread_id", "sms_thread_id"},
	{"archive_status", "archive_status"},
	{"participant_count", "participant_count"},
	{"IS_ENTERPRISE", "is_enterprise"},
}

// threadOf states the conversation join: the thread (its id as stored, the cleaned name as title, the group flag and the
// member addresses), the conversation's raw keys with their time statuses, and the members read once per conversation
// (membersOf). The members are the conversation's current non-self parties with an address, or nil when the conversation
// is not decided.
func threadOf(m parse.MapContext, e *env, row parse.Row, c rowrun.Cols, pc rowrun.Cols, raw, unknown map[string]any) (*message.Thread, []party, error) {
	cid, ck := intCellOf(row, c.At("conversation_id"))
	rowrun.Flag(unknown, row, "conversation_id", c.At("conversation_id"), ck)
	switch ck {
	case rowrun.CellValue:
		raw["conversation_id"] = cid
	case rowrun.CellNull:
		raw["conversation_state"] = stateNull
		return nil, nil, nil
	case rowrun.CellMissing:
		return nil, nil, nil
	default:
		raw["conversation_state"] = stateUndecided
		return nil, nil, nil
	}
	th := &message.Thread{ID: strconv.FormatInt(cid, 10)}
	crow, found, err := m.Get(tableConversations, cid)
	switch {
	case err != nil && joinUndecided(err):
		undecidedJoin(raw, tableConversations)
		raw["conversation_state"] = stateUndecided
		return th, nil, nil
	case err != nil:
		return nil, nil, err
	case !found:
		raw["conversation_state"] = stateNoConv
		return th, nil, nil
	}
	cc := rowrun.Resolve(m, tableConversations, "name", "sms_thread_id", "archive_status", "participant_count", "IS_ENTERPRISE", "sort_timestamp", "last_read_timestamp")
	for _, kk := range conversationIntKeys {
		v, k := intCellOf(crow, cc.At(kk[0]))
		rowrun.Flag(unknown, crow, "conversations."+kk[0], cc.At(kk[0]), k)
		if k == rowrun.CellValue {
			raw[kk[1]] = v
		}
	}
	for _, col := range []string{"sort_timestamp", "last_read_timestamp"} {
		v, k := intCellOf(crow, cc.At(col))
		rowrun.Flag(unknown, crow, "conversations."+col, cc.At(col), k)
		if k == rowrun.CellValue {
			rowrun.MillisFrom1973(v).Record(raw, col)
		}
	}
	nb, nk := textCellOf(crow, cc.At("name"))
	rowrun.Flag(unknown, crow, "conversations.name", cc.At("name"), nk)
	if nk == rowrun.CellValue {
		th.Title = cleanField(raw, "thread.title", nb, maxName)
	}

	set, err := membersOf(m, e.Members, cid, pc)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range set.Joins {
		undecidedJoin(raw, t)
	}
	for k, v := range set.Unknown {
		if _, dup := unknown[k]; !dup {
			unknown[k] = v
		}
	}
	if set.Undecided {
		raw["members_state"] = stateUndecided
		return th, nil, nil
	}
	raw["members_total"] = int64(set.Total)
	if set.Cut {
		raw["members_cut"] = true
	}
	if set.Dangling > 0 {
		raw["members_dangling"] = int64(set.Dangling)
	}
	if set.Without > 0 {
		raw["members_without_address"] = int64(set.Without)
	}
	if len(set.Blocked) > 0 {
		blocked := make([]any, len(set.Blocked))
		for i, id := range set.Blocked {
			blocked[i] = id
		}
		raw["blocked_member_ids"] = blocked
	}
	if set.SelfUndecided {
		raw["self_state"] = stateUndecided
		raw["group_state"] = stateUndecided
	} else {
		g := set.Total > 1
		th.Group = &g
	}
	if set.Total == 0 {
		raw["members_state"] = "none"
	}
	for _, p := range set.Parties {
		th.Members = append(th.Members, p.Address)
	}
	return th, set.Parties, nil
}
