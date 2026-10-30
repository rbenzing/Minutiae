package iossms

import (
	"errors"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// address is the address of a handle row, or why there is none (raw.handle_state): "no-handle-row" for a
// dangling id, "no-handle-table" when the database has no handle table, "no-address" for an empty, NULL or
// unreadable id, "address-too-long" past the contract's 1024 bytes (an address is never cut: a cut number is
// another number).
type address struct {
	addr   string
	state  string
	flags  []string
	rawB64 string
}

func handleAddress(m parse.MapContext, id int64) (address, error) {
	row, found, err := m.Get("handle", id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return address{state: "no-handle-table"}, nil
	case err != nil:
		return address{}, err
	case !found:
		return address{state: "no-handle-row"}, nil
	}
	b, k := rowrun.TextCell(row, m.Column("handle", "id"))
	if k != rowrun.CellValue || len(b) == 0 {
		return address{state: "no-address"}, nil
	}
	cl := common.CleanText(b, maxText)
	if len(cl.Text) > maxAddr {
		return address{state: "address-too-long"}, nil
	}
	return address{addr: cl.Text, flags: cl.Flags, rawB64: cl.RawB64}, nil
}

func participantOf(role, addr string) message.Participant {
	return message.Participant{Role: role, Address: addr, Kind: common.AddressKind(addr)}
}

// chatInfo is what the chat joins say about a message.
type chatInfo struct {
	thread     *message.Thread
	members    []string // the members of the only chat (handle ROWID order), when membersOK
	one        bool     // the message is in exactly one chat
	membersOK  bool     // the member list is the stored answer
	unresolved int64    // members whose handle has no usable address
	undecidedJ bool     // the chat_message_join answer cannot be trusted
}

// markUndecided lists table in raw.join_undecided (in the order found, once).
func markUndecided(raw map[string]any, table string) {
	cur, _ := raw["join_undecided"].([]any)
	if !slices.Contains(cur, any(table)) {
		raw["join_undecided"] = append(cur, table)
	}
}

// chatOf resolves the chat of message id (4E-R20). Every gap is stated in raw and never filled: no join row
// is "no-chat-join", several "several-chats" with raw.chat_ids, a chat id without a row "no-chat-row", a
// missing optional table "no-join-table" (or "no-chat-table"), and a join whose answer is not decided lists its
// table in raw.join_undecided and gives no thread.
func chatOf(m parse.MapContext, id int64, raw, unknown map[string]any) (chatInfo, error) {
	var ci chatInfo
	rows, undecided, err := rowrun.Match(m, "chat_message_join", "message_id", id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		raw["thread_state"] = "no-join-table"
		return ci, nil
	case err != nil:
		return ci, err
	}
	cc := m.Column("chat_message_join", "chat_id")
	var ids []int64
	for _, r := range rows {
		v, k := rowrun.IntCell(r, cc)
		if k != rowrun.CellValue {
			undecided = true
			continue
		}
		ids = append(ids, v)
	}
	if undecided {
		markUndecided(raw, "chat_message_join")
		ci.undecidedJ = true
		return ci, nil
	}
	ids = uniqueSorted(ids)
	switch len(ids) {
	case 0:
		raw["thread_state"] = "no-chat-join"
		return ci, nil
	case 1:
	default:
		raw["thread_state"] = "several-chats"
		list := ids
		if len(list) > maxList {
			raw["chat_ids_total"], raw["chat_ids_truncated"] = int64(len(list)), true
			list = list[:maxList]
		}
		out := make([]any, len(list))
		for i, v := range list {
			out[i] = v
		}
		raw["chat_ids"] = out
		return ci, nil
	}
	ci.one = true
	cid := ids[0]
	ci.members, ci.membersOK, ci.unresolved, err = membersOf(m, cid, raw)
	if err != nil {
		return ci, err
	}
	th := &message.Thread{ID: strconv.FormatInt(cid, 10)}
	ci.thread = th
	crow, found, err := m.Get("chat", cid)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		raw["thread_state"] = "no-chat-table"
		return ci, nil
	case err != nil:
		return ci, err
	case !found:
		raw["thread_state"] = "no-chat-row"
		return ci, nil
	}
	cc2 := rowrun.Resolve(m, "chat", "style", "display_name")
	if title, ok := textCell(crow, cc2, "display_name", "thread.title", raw, unknown); ok && title != "" {
		th.Title = title
	}
	if style, k := rowrun.IntCell(crow, cc2.At("style")); k == rowrun.CellValue && (style == 43 || style == 45) {
		g := style == 43
		th.Group = &g
	}
	if ci.membersOK && len(ci.members) > 0 {
		th.Members = ci.members
	}
	return ci, nil
}

// membersOf lists the addresses of the members of chat cid, in handle ROWID order (at most 1000). ok is false
// when the list is not the stored answer: the table is missing (raw.members_state "no-join-table") or its answer
// is not decided (raw.join_undecided). A member whose handle has no usable address is counted in
// raw.members_unresolved, never dropped silently.
func membersOf(m parse.MapContext, cid int64, raw map[string]any) (members []string, ok bool, unresolved int64, err error) {
	rows, undecided, err := rowrun.Match(m, "chat_handle_join", "chat_id", cid)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		raw["members_state"] = "no-join-table"
		return nil, false, 0, nil
	case err != nil:
		return nil, false, 0, err
	}
	hc := m.Column("chat_handle_join", "handle_id")
	var hids []int64
	for _, r := range rows {
		v, k := rowrun.IntCell(r, hc)
		if k != rowrun.CellValue {
			undecided = true
			continue
		}
		hids = append(hids, v)
	}
	if undecided {
		markUndecided(raw, "chat_handle_join")
		return nil, false, 0, nil
	}
	slices.Sort(hids)
	if len(hids) > maxList {
		raw["members_total"], raw["members_truncated"] = int64(len(hids)), true
		hids = hids[:maxList]
	}

	for _, h := range hids {
		a, err := handleAddress(m, h)
		if err != nil {
			return nil, false, 0, err
		}
		if a.addr == "" {
			unresolved++
			continue
		}
		if len(a.flags) > 0 {
			markText(raw, "thread.members")
		}
		members = append(members, a.addr)
	}
	if unresolved > 0 {
		raw["members_unresolved"] = unresolved
	}
	return members, true, unresolved, nil
}

// uniqueSorted returns ids sorted and without repeats, in O(n log n).
func uniqueSorted(ids []int64) []int64 {
	slices.Sort(ids)
	return slices.Compact(ids)
}
