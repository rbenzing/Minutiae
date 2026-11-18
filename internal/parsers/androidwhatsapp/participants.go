package androidwhatsapp

import (
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// participantsOf decides who a message is from or to (spec 6.4.3, R3). The device owner's JID is not in msgstore.db,
// so an outgoing message has a "to" and no "from". A group sender is never the group. An incoming status update has no
// attributed author. An address whose JID string is absent is never composed from its parts: the parts go to
// raw.from_jid or raw.to_jid and the participants are unknown.
func participantsOf(f *msgFacts, dir string, raw map[string]any) (ps []message.Participant, unknown bool) {
	chat := f.Chat.Str
	if s := f.Sender.State; s == stateNoJIDRow || s == stateNoRawString || s == stateUndecided {
		raw["sender_state"] = s
	}
	group := f.Chat.group()
	isGroup := group != nil && *group
	switch {
	case dir == message.DirOut && chat != "":
		return []message.Participant{{Role: message.RoleTo, Address: chat}}, false
	case dir == message.DirIn && f.Chat.isStatus():
		return nil, true
	case dir == message.DirIn && isGroup:
		if s := f.Sender.Str; s != "" {
			return []message.Participant{{Role: message.RoleFrom, Address: s}}, false
		}
		raw["sender_state"] = f.Sender.State
		roleJID(raw, "from_jid", f.Sender.Parts)
		return nil, true
	case dir == message.DirIn && chat != "":
		return []message.Participant{{Role: message.RoleFrom, Address: chat}}, false
	}
	switch dir {
	case message.DirIn:
		roleJID(raw, "from_jid", f.Chat.Parts)
	case message.DirOut:
		roleJID(raw, "to_jid", f.Chat.Parts)
	}
	return nil, true
}

func roleJID(raw map[string]any, key string, parts map[string]any) {
	if parts != nil {
		raw[key] = parts
	}
}
