package androidtelegram_test

import (
	"encoding/binary"
	"encoding/json"
	"iter"
	"maps"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// A small TL writer for Go-built rows (spec 6.5: little-endian ints, the short string form, zero padding), written
// from the wire facts and TL_message's field order in constructors.tsv (TLRPC.java:60187@A-HEAD), independently of
// the parser's decoder.

const (
	idTLMessage = 0x7600b9d3
	idPeerUser  = 0x59511722
	// TL_message flag bits (constructors.tsv)
	bitOut           = 1 << 1
	bitFwdFrom       = 1 << 2
	bitFromID        = 1 << 8
	bitReplyMarkup   = 1 << 6
	bitMedia         = 1 << 9
	bitPost          = 1 << 14
	bitEditDate      = 1 << 15
	bitFromScheduled = 1 << 18
	bitReactions     = 1 << 20
	bitEditHide      = 1 << 21
)

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func le64(v int64) []byte { return binary.LittleEndian.AppendUint64(nil, uint64(v)) }

// tlStr is a TL string: its length byte (or 0xFE and 3 length bytes from 254 bytes on), the bytes and zero padding
// to a multiple of 4.
func tlStr(s string) []byte {
	b := []byte{byte(len(s))}
	if len(s) > 253 {
		b = []byte{0xfe, byte(len(s)), byte(len(s) >> 8), byte(len(s) >> 16)}
	}
	b = append(b, s...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func peerUser(id int64) []byte { return append(le32(idPeerUser), le64(id)...) }

// tlMessage is one TL_message: flags (out, from_scheduled and the gates of from_id, fwd_from, media, reply_markup and
// edit_date are set from the fields), flags2 0, id, [from_id], peer_id, [fwd_from], date, message, [media],
// [reply_markup], [edit_date], then tail (an app-local trailer, spec 6.5, or the fields after edit_date with their
// bits set in flags, such as F.20 reactions) as given. fwd, media and markup are the encoded objects, ID first;
// fromPeer and peerObj, when set, are the encoded from_id and peer_id objects in place of a TL_peerUser of from and
// peer. editSet writes F.15 and editDate even when editDate is 0 (plan 4I4b Task 3).
type tlMessage struct {
	flags, flags2     uint32
	id                int32
	from, peer        int64
	date, editDate    int32
	editSet           bool
	text              string
	fwd, media        []byte
	markup            []byte
	fromPeer, peerObj []byte
	tail              []byte
}

func (m tlMessage) bytes() []byte {
	flags := m.flags
	if m.from != 0 || m.fromPeer != nil {
		flags |= bitFromID
	}
	if m.editDate != 0 || m.editSet {
		flags |= bitEditDate
	}
	if m.markup != nil {
		flags |= bitReplyMarkup
	}
	if m.fwd != nil {
		flags |= bitFwdFrom
	}
	if m.media != nil {
		flags |= bitMedia
	}
	b := append(le32(idTLMessage), le32(flags)...)
	b = append(b, le32(m.flags2)...)
	b = append(b, le32(uint32(m.id))...)
	switch {
	case m.fromPeer != nil:
		b = append(b, m.fromPeer...)
	case m.from != 0:
		b = append(b, peerUser(m.from)...)
	}
	if m.peerObj != nil {
		b = append(b, m.peerObj...)
	} else {
		b = append(b, peerUser(m.peer)...)
	}
	b = append(b, m.fwd...)
	b = append(b, le32(uint32(m.date))...)
	b = append(b, tlStr(m.text)...)
	b = append(b, m.media...)
	b = append(b, m.markup...)
	if m.editDate != 0 || m.editSet {
		b = append(b, le32(uint32(m.editDate))...)
	}
	return append(b, m.tail...)
}

// The DDL of the Go-built message tables: the A-HEAD messages_v2 columns (spec 6.3) and the v64 messages columns
// (facts.tsv columns.messages.v64), without the keys and indexes the mapping never reads.
const (
	ddlV2Full = `CREATE TABLE messages_v2 (mid INTEGER, uid INTEGER, read_state INTEGER, send_state INTEGER, date INTEGER, ` +
		`data BLOB, out INTEGER, ttl INTEGER, media INTEGER, replydata BLOB, imp INTEGER, mention INTEGER, forwards INTEGER, ` +
		`replies_data BLOB, thread_reply_id INTEGER, is_channel INTEGER, reply_to_message_id INTEGER, custom_params BLOB, ` +
		`group_id INTEGER, reply_to_story_id INTEGER)`
	ddlV1Full = `CREATE TABLE messages (mid INTEGER PRIMARY KEY, uid INTEGER, read_state INTEGER, send_state INTEGER, ` +
		`date INTEGER, data BLOB, out INTEGER, ttl INTEGER, media INTEGER, replydata BLOB, imp INTEGER, mention INTEGER)`
	baseDate = 1700000000 // 2023-11-14T22:13:20Z, the fixtures' D
)

// msgCells are the cells of one Go-built message row; the zero value of every other column is the fixtures' default.
type msgCells struct {
	mid, uid                   int64
	nullMid, nullUID           bool // store NULL in mid or uid
	midCell, uidCell           any  // when set, the stored mid or uid cell (a REAL, a NaN, a TEXT)
	read, send, date, out, ttl any
	data, replydata            any
	repliesData                []byte // the replies_data cell (NULL when nil)
}

// key is the stored mid and uid (NULL when asked).
func (c msgCells) key() (mid, uid any) {
	mid, uid = c.mid, c.uid
	if c.nullMid {
		mid = nil
	}
	if c.nullUID {
		uid = nil
	}
	if c.midCell != nil {
		mid = c.midCell
	}
	if c.uidCell != nil {
		uid = c.uidCell
	}
	return mid, uid
}

// v2Values are the cells of a messages_v2 row in declared order (ttl 0, media -1, the rest 0 or NULL).
func (c msgCells) v2Values() []any {
	mid, uid := c.key()
	return []any{
		mid, uid, orDefault(c.read, int64(3)), orDefault(c.send, int64(0)), orDefault(c.date, int64(baseDate)),
		c.data, orDefault(c.out, int64(0)), orDefault(c.ttl, int64(0)), int64(-1), c.replydata, int64(0), int64(0), int64(0),
		c.repliesCell(), int64(0), int64(0), int64(0), nil, nil, int64(0),
	}
}

// v1Values are the cells of a messages row (mid is the rowid alias: stored NULL in the record).
func (c msgCells) v1Values() []any {
	_, uid := c.key()
	return []any{
		nil, uid, orDefault(c.read, int64(3)), orDefault(c.send, int64(0)), orDefault(c.date, int64(baseDate)),
		c.data, orDefault(c.out, int64(0)), orDefault(c.ttl, int64(0)), int64(-1), c.replydata, int64(0), int64(0),
	}
}

// nullCell asks orDefault for a stored NULL where a nil means the default.
type nullCell struct{}

func orDefault(v, def any) any {
	switch v.(type) {
	case nil:
		return def
	case nullCell:
		return nil
	}
	return v
}

// goBuiltMessages is a Go-built database at userVersion with users and the message tables given rows (messages_v2
// rows by their rowid order, from 1; messages rows by their mid).
func goBuiltMessages(userVersion int32, v1, v2 []msgCells) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: userVersion})
	b.CreateTable("users", ddlUsers)
	if v2 != nil {
		t := b.CreateTable("messages_v2", ddlV2Full)
		for i, c := range v2 {
			t.Insert(int64(i+1), c.v2Values()...)
		}
	}
	if v1 != nil {
		t := b.CreateTable("messages", ddlV1Full)
		for _, c := range v1 {
			t.Insert(c.mid, c.v1Values()...)
		}
	}
	return b.Bytes()
}

// byRow indexes records by "<table>/<rowid>" of their locator.
func byRow(recs []records.Record) map[string]records.Record {
	out := map[string]records.Record{}
	for _, r := range recs {
		out[locatorRow(r.Locator)] = r
	}
	return out
}

// rawOf is a record's payload.raw.
func rawOf(r records.Record) map[string]any {
	raw, _ := r.Payload["raw"].(map[string]any)
	return raw
}

// parseFixture parses a committed variant at the app path.
func parseFixture(t *testing.T, variant string) parsertest.Result {
	t.Helper()
	res := parseAt(t, remoteApp, fixtureFiles(t, variant))
	if res.Err != nil {
		t.Fatalf("%s: Parse err %v", variant, res.Err)
	}
	return res
}

// mapsKeys is the keys of m.
func mapsKeys(m map[string]any) iter.Seq[string] { return maps.Keys(m) }

// jsonText is v as canonical JSON (an int64 and a json.Number of the same value alike).
func jsonText(v any) string {
	bs, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(bs)
}

// repliesCell is the stored replies_data cell: NULL for none.
func (c msgCells) repliesCell() any {
	if c.repliesData == nil {
		return nil
	}
	return c.repliesData
}
