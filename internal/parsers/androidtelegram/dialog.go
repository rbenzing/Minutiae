package androidtelegram

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// dialogClass is the class of a dialog (spec 8.6, 4I4-P11), stated in raw.dialog_class.
type dialogClass string

// The dialog classes: the uid alone decides user, secret, folder, negative and unknown; a negative dialog is
// resolved to chat, channel or community by its evidence (resolveNegative).
const (
	classUser      dialogClass = "user"
	classSecret    dialogClass = "secret"
	classFolder    dialogClass = "folder"
	classNegative  dialogClass = "negative"
	classChat      dialogClass = "chat"
	classChannel   dialogClass = "channel"
	classCommunity dialogClass = "community"
	classUnknown   dialogClass = "unknown"
)

// addressClasses are the classes a participant address names from a TL peer or an inline id (T4): the Peer
// constructors know users, basic groups and channels only.
func addressClasses() []dialogClass { return []dialogClass{classUser, classChat, classChannel} }

// grouped says the dialog is a group conversation: a basic group, a channel or a community (spec 8.6 thread.group).
func (c dialogClass) grouped() bool {
	return c == classChat || c == classChannel || c == classCommunity
}

// The join states of spec 8.6 (the plan's "State vocabularies"): no-<table>-row, zero-key (B13: a key of 0 that finds
// no row), blob-undecided, class-mismatch, undecided (with raw.join_undecided) and not-joined-recovered (4I4-P6).
const (
	joinZeroKey      = "zero-key"
	joinBlobUnknown  = "blob-undecided"
	joinMismatch     = "class-mismatch"
	joinRecovered    = "not-joined-recovered"
	senderAbsent     = "absent"
	senderBlobUnread = "blob-undecided"
	senderChannel    = "channel-post"
)

const minInt64 = -1 << 63

// classOfUID is the class the uid alone decides (4I4-P11, facts.tsv predicate.secret_dialog and
// predicate.folder_dialog, DO:30-36,105-111): a positive uid with bit 62 set is secret, else with bit 61 set a folder,
// else a user; a negative uid (not MinInt64) is a basic group, channel or community, undecided here; 0 and MinInt64
// are unknown (never negated).
func classOfUID(uid int64) dialogClass {
	switch {
	case uid > 0 && uid>>62&1 == 1:
		return classSecret
	case uid > 0 && uid>>61&1 == 1:
		return classFolder
	case uid > 0:
		return classUser
	case uid < 0 && uid != minInt64:
		return classNegative
	}
	return classUnknown
}

// pre84Secret says a row of the old message table matches facts.tsv predicate.pre84_secret_uid: the low 32 bits of
// uid, cast to int, are 0 (no guard on the high part, T1 c6; a uid of 0 matches too).
func pre84Secret(sourceTable string, uid int64) bool {
	return sourceTable == tableMessages && uint32(uid) == 0
}

// dialogOf is the effective dialog of a row (F10): its class and, for a secret dialog, the enc_chats key. A
// "messages" row matching predicate.pre84_secret_uid is secret with the chat id uid >> 32 as a signed int (the pin);
// every other row takes classOfUID, and a secret one the key encChatID(uid). The class and the key are never derived
// from the uid alone where the source table decides (a pre-84 high-id row is secret, key 77).
func dialogOf(sourceTable string, uid int64) (dialogClass, int64) {
	if pre84Secret(sourceTable, uid) {
		return classSecret, int64(int32(uid >> 32))
	}
	c := classOfUID(uid)
	if c == classSecret {
		return c, encChatID(uid)
	}
	return c, 0
}

// dialogAddress is the dialog's own participant address (4I4-P11, T4): user:<uid>, chat:/channel:/community:<-uid>
// (the negation is the DO convention, never an offset), a secret dialog user:<enc_chats.user> when the join gave it;
// "" for a folder, an unresolved negative dialog, an unknown one or a secret one without its user.
func dialogAddress(c dialogClass, uid, encUser int64, encOK bool) string {
	switch {
	case c == classUser:
		return string(c) + ":" + strconv.FormatInt(uid, 10)
	case c.grouped():
		return string(c) + ":" + strconv.FormatInt(-uid, 10)
	case c == classSecret && encOK:
		return string(classUser) + ":" + strconv.FormatInt(encUser, 10)
	}
	return ""
}

// peerAddress is the participant address of the one peer read at path (addressesAt): its address and class. ok is
// false when none was read or more than one.
func peerAddress(v view, path string) (string, dialogClass, bool) {
	addrs, unresolved := addressesAt(v, path)
	if len(addrs) != 1 || len(unresolved) != 0 {
		return "", "", false
	}
	cls, _, _ := strings.Cut(addrs[0], ":")
	return addrs[0], dialogClass(cls), true
}

// chatClassOf is the dialog class a chats.data constructor names (4I4-P11): TL_chat* (TL_chatForbidden*,
// TL_chatEmpty*) chat, TL_channel* (TL_channelForbidden*) channel, TL_community* community.
func chatClassOf(name string) dialogClass {
	switch {
	case strings.HasPrefix(name, "TL_chat"):
		return classChat
	case strings.HasPrefix(name, "TL_channel"):
		return classChannel
	case strings.HasPrefix(name, "TL_community"):
		return classCommunity
	}
	return ""
}

// undecidedJoin lists a join in raw.join_undecided (sorted, once; restated from androidcalendar/text.go, F29).
func undecidedJoin(raw map[string]any, table string) {
	cur, _ := raw["join_undecided"].([]any)
	if slices.Contains(cur, any(table)) {
		return
	}
	cur = append(cur, table)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["join_undecided"] = cur
}

// joinGet is the rowid-alias join of spec 8.6: the row of table at key, or the state of a miss: no-<table>-row, or
// zero-key when the key is 0 (B13). A join that cannot be trusted (a guarded table, a table the database lacks or
// that cannot be read, an undecidable key) is "undecided" and named in raw.join_undecided, never a miss. Any other
// error (the budget, a cancel, a sticky I/O failure) fails the job.
func joinGet(m parse.MapContext, table string, key int64, raw map[string]any) (parse.Row, string, error) {
	row, found, err := m.Get(table, key)
	switch {
	case err != nil && (errors.Is(err, sqlitedb.ErrNoSuchTable) || dataDamage(err)):
		undecidedJoin(raw, table)
		return nil, stateUndecided, nil
	case err != nil:
		return nil, "", err
	case found:
		return row, "", nil
	case key == 0:
		return nil, joinZeroKey, nil
	}
	return nil, "no-" + table + "-row", nil
}

// chatJoin is what the chats row of a negative dialog says: its class (from the decoded constructor; "" when
// undecided), its cleaned title, and the state of the join when it gives no title.
type chatJoin struct {
	class dialogClass
	title string
	state string
}

// joinChat joins the chats row of a negative dialog by -uid (spec 8.6) and decodes its data blob (raw chat_data_*,
// spec 8.4), charging bud and freeing the charge before it returns (F8): the title is read only from the blob,
// never from chats.name (a lowercase search string, MS:11047). A row whose blob is NULL, unreadable, or does not
// reach the title is blob-undecided.
func joinChat(m parse.MapContext, uid int64, raw map[string]any, tab tl.Table, bud tl.Budget) (chatJoin, error) {
	row, state, err := joinGet(m, tableChats, -uid, raw)
	if err != nil || row == nil {
		return chatJoin{state: state}, err
	}
	b, k := blobCell(row, m.Column(tableChats, colData))
	if k != rowrun.CellValue {
		return chatJoin{state: joinBlobUnknown}, nil
	}
	v, st := decodeBlob(raw, "chat_data", b, typeChat, tab, bud)
	defer func() {
		if bud != nil {
			bud.Free(v.r.Charged())
		}
	}()
	top, ok := topOf(v, tab, typeChat, st)
	if !ok {
		return chatJoin{state: joinBlobUnknown}, nil
	}
	j := chatJoin{class: chatClassOf(top.Name)}
	title, read := v.Str("title")
	if j.class == "" || !read {
		j.state = joinBlobUnknown
		return j, nil
	}
	cl := cleanTitle(raw, title)
	j.title = cl
	return j, nil
}

// cleanTitle is a chat title cleaned at 1 KiB (C4), named thread.title in raw.text_flags when altered or cut.
func cleanTitle(raw map[string]any, b []byte) string {
	tmp := map[string]any{}
	return cleanInto(tmp, raw, "title", "thread.title", b, maxName)
}

// resolveNegative decides a negative dialog's class (4I4-P11) from its evidence, in order: the blob's peer_id class,
// the is_channel column when non-zero (channel), and the chats row's constructor class. The sources available must
// all name the same group class; a disagreement, or a source that names no group class (a user peer in a negative
// dialog), is raw.dialog_class_state "conflict" and the class stays negative. No source leaves it negative.
func resolveNegative(raw map[string]any, peer dialogClass, isChannel int64, isChannelOK bool, chat dialogClass) dialogClass {
	var sources []dialogClass
	if peer != "" {
		sources = append(sources, peer)
	}
	if isChannelOK && isChannel != 0 {
		sources = append(sources, classChannel)
	}
	if chat != "" {
		sources = append(sources, chat)
	}
	if len(sources) == 0 {
		return classNegative
	}
	for _, s := range sources {
		if s != sources[0] || !s.grouped() {
			raw["dialog_class_state"] = stateConflict
			return classNegative
		}
	}
	return sources[0]
}

// dialog is the effective dialog of one message row (F10): computed once by resolveDialog and taken by the
// participants, the thread and the Summary.
type dialog struct {
	class   dialogClass
	uid     int64
	encID   int64 // the enc_chats key of a secret dialog
	encUser int64 // enc_chats.user of a secret dialog, when encOK
	encOK   bool
}

// resolveDialog computes the dialog of a row whose uid was read (spec 8.6): the class (dialogOf; a negative one
// resolved through the peer, is_channel and the chats join), the thread {id, title, group} with its join states, and
// for a secret dialog raw.secret_chat, raw.enc_chat_id and the counterparty from enc_chats. A nil e is the typed
// pass (4I4-P6): no join runs and every join state is not-joined-recovered.
func resolveDialog(m parse.MapContext, e *env, r *msgRow, v view, hasTop bool, tab tl.Table, bud tl.Budget, uid int64) (dialog, *message.Thread, error) {
	raw := r.raw
	c, encID := dialogOf(r.table, uid)
	d := dialog{class: c, uid: uid, encID: encID}
	th := &message.Thread{ID: strconv.FormatInt(uid, 10)}
	if pre84Secret(r.table, uid) {
		raw["dialog_basis"] = dialogBasisPre84
	}
	switch c {
	case classUser:
		state := joinRecovered
		if e != nil {
			var err error
			if _, state, err = joinGet(m, tableUsers, uid, raw); err != nil {
				return d, nil, err
			}
		}
		if state != "" {
			raw["thread_user_state"] = state // users.name is a search string: the user dialog takes no title
		}
	case classSecret:
		raw["secret_chat"], raw["enc_chat_id"] = true, encID
		user, ok, err := encChatUser(m, e, encID, r)
		if err != nil {
			return d, nil, err
		}
		d.encUser, d.encOK = user, ok
	case classNegative:
		var peer dialogClass
		if hasTop {
			_, peer, _ = peerAddress(v, "peer_id")
		}
		isCh, isChOK := r.cell("is_channel")
		chat := chatJoin{state: joinRecovered}
		if e != nil {
			var err error
			if chat, err = joinChat(m, uid, raw, tab, bud); err != nil {
				return d, nil, err
			}
		}
		d.class = resolveNegative(raw, peer, isCh, isChOK, chat.class)
		switch {
		case chat.state != "":
			raw["thread_title_state"] = chat.state
		case chat.class != d.class:
			raw["thread_title_state"] = joinMismatch
		case chat.title != "":
			th.Title = chat.title
		}
	}
	switch {
	case d.class == classUser || d.class == classSecret:
		th.Group = new(false)
	case d.class.grouped():
		th.Group = new(true)
	}
	return d, th, nil
}
