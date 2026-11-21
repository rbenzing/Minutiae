package androidtelegram

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Secret chats and key material (spec 9, call 6, T1, F9). This file holds every access to enc_chats: the table is
// joined by its rowid alias and read in two columns, uid and user; data (TL EncryptedChat) is never decoded, and no
// other column (the key, its hashes and fingerprints, the sequence counters) is ever named, requested, scanned or
// counted beyond the table's row count. TestTelegramEncChatsColumnsAllowlisted pins it.

// tableEncChats is the only "enc_chats" literal of the package (F9).
const tableEncChats = "enc_chats"

// encChatsColumns are the only enc_chats columns the parser ever names (spec 9, call 6, F9): the key (the rowid
// alias) and the counterparty. A function, not the plan's package-level var: a package slice handed to a Req would
// escape, and a parser holds no mutable state (P5, TestParserPackagesHaveNoMutableState; ruling T6 c1).
func encChatsColumns() []string { return []string{"uid", "user"} }

// encChatID is the enc_chats key of a secret dialog id (4I4-P12, facts.tsv cast.encrypted_chat_id, DO:117-119): the
// low 32 bits of uid, sign-extended when the pinned cast is to int (encChatIDSigned), else unsigned.
func encChatID(uid int64) int64 {
	if encChatIDSigned {
		return int64(int32(uid))
	}
	return int64(uint32(uid))
}

// encChatUser joins the enc_chats row of a secret dialog by its key encID (from dialogOf, never from the uid alone;
// F10) and reads its user column only (spec 8.6, 9): the counterparty's user id, ok when read. A miss states
// raw.counterparty_state no-enc_chats-row (zero-key for a key of 0, B13); an untrusted join "undecided" with
// raw.join_undecided; a user cell that is NULL, of another class or unreadable is listed in unknown_columns as
// enc_chats.user. A nil e is the typed pass (4I4-P6): no join, counterparty_state not-joined-recovered.
func encChatUser(m parse.MapContext, e *env, encID int64, r *msgRow) (int64, bool, error) {
	if e == nil {
		r.raw["counterparty_state"] = joinRecovered
		return 0, false, nil
	}
	row, state, err := joinGet(m, tableEncChats, encID, r.raw)
	if err != nil || row == nil {
		if state != "" {
			r.raw["counterparty_state"] = state
		}
		return 0, false, err
	}
	idx := m.Column(tableEncChats, encChatsColumns()[1])
	name := tableEncChats + "." + encChatsColumns()[1]
	user, k := intCellOf(row, idx)
	switch k {
	case rowrun.CellValue:
		return user, true, nil
	case rowrun.CellNull:
		r.unknown[name] = unknownNull
	case rowrun.CellOther:
		r.unknown[name] = unknownTypeMismatch
	default: // missing or unreadable: the decoder's state of the cell
		r.unknown[name] = rowrun.StateOf(row, idx).String()
	}
	return 0, false, nil
}
