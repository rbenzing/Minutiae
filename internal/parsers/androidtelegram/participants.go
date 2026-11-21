package androidtelegram

import (
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// participantsOf are the participants of spec 8.6, row by row, and whether they are unknown:
//   - outgoing: [{to: <the dialog's address>}] (a secret dialog's is user:<enc_chats.user>); none known: unknown;
//   - incoming in a user dialog: [{from: user:<uid>}] (the dialog id is the user id, DO:71-82);
//   - incoming in a secret, group, channel or community dialog: [{from: <from_id address>}] when the blob read it
//     (SCH:851-852 for a secret chat); a channel post (post set, no from_id) [{from: channel:<id>}] with
//     raw.sender_basis "channel-post"; else unknown with raw.sender_state "absent" (the blob's flags say no from_id)
//     or "blob-undecided" (the blob did not reach it). The app's synthesized from_id (TLRPC:59550-59559) is never
//     made here: no sender is ever invented;
//   - a folder, an unresolved negative or an unknown dialog, or an unknown direction: unknown (an unresolved negative
//     dialog keeps its decoded sender address in raw.from_id).
//
// An inline from_id of a class the table leaves unresolved gives no address: raw.from_id_id (the id as stored) and
// raw.from_id_class_state "contradictory" (4I4-P11, T5).
func participantsOf(v view, hasTop bool, state, dir string, d dialog, raw map[string]any) ([]message.Participant, bool) {
	own := dialogAddress(d.class, d.uid, d.encUser, d.encOK)
	switch dir {
	case message.DirOut:
		if own == "" {
			return nil, true
		}
		return []message.Participant{{Role: message.RoleTo, Address: own}}, false
	case message.DirIn:
	default:
		return nil, true
	}
	switch {
	case d.class == classUser:
		return []message.Participant{{Role: message.RoleFrom, Address: own}}, false
	case d.class == classNegative:
		// an unresolved negative dialog keeps the decoded sender in raw, never as a participant (M5)
		if a, _, ok := peerAddress(v, "from_id"); hasTop && ok {
			raw["from_id"] = a
		}
		return nil, true
	case d.class != classSecret && !d.class.grouped():
		return nil, true
	}
	if hasTop {
		from, unresolved := addressesAt(v, "from_id")
		switch {
		case len(from) == 1 && len(unresolved) == 0:
			return []message.Participant{{Role: message.RoleFrom, Address: from[0]}}, false
		case len(from) == 0 && len(unresolved) == 1:
			raw["from_id_id"], raw["from_id_class_state"] = unresolved[0], stateContradictory
			return nil, true
		}
		if d.class == classChannel && v.Has("flags") && v.True("post") {
			raw["sender_basis"] = senderChannel
			return []message.Participant{{Role: message.RoleFrom, Address: own}}, false
		}
	}
	raw["sender_state"] = senderBlobUnread
	if fromRead(v, hasTop, state, raw) {
		raw["sender_state"] = senderAbsent
	}
	return nil, true
}

// fromRead says the blob decided its from_id: the top constructor is known, its flags word was read, and the decode
// either read it whole or stopped outside from_id where it is not among the undecided fields (spec 8.4): an absent
// from_id is then the blob's own fact. A stop inside from_id leaves it unread, never absent.
func fromRead(v view, hasTop bool, state string, raw map[string]any) bool {
	switch {
	case !hasTop || !v.Has("flags"):
		return false
	case state == stateDecoded:
		return true
	case v.r.Stop != nil && headOf(v.r.Stop.Path) == "from_id":
		return false
	}
	und, _ := raw[colData+"_undecided"].(string)
	return !slices.Contains(strings.Split(und, ","), "from_id")
}

// messageSummary is the Summary of a message record (spec 9, Q13): the channel "telegram secret" for a secret
// dialog (a flag of the record, no contract field: 4I D9), the direction, the first participant's address and the
// text.
func messageSummary(d dialog, dir string, parts []message.Participant, body string) string {
	ch := channelTelegram
	if d.class == classSecret {
		ch = channelTelegram + " " + string(classSecret)
	}
	cp := ""
	if len(parts) > 0 {
		cp = parts[0].Address
	}
	return message.Summary(ch, dir, cp, body)
}

// peerRaw states the blob's peer_id as an address in raw.peer_id (spec 8.5 raw list, 4I4-P11).
func peerRaw(v view, raw map[string]any) {
	if a, _, ok := peerAddress(v, "peer_id"); ok {
		raw["peer_id"] = a
	}
}
