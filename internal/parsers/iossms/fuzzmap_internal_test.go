package iossms

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// Cell storage classes of the fuzzed rows.
const (
	cNull byte = iota
	cInt
	cFloat
	cText
	cBlob
	numKinds
)

type cell struct {
	kind byte
	i    int64
	f    float64
	b    []byte
}

func txt(s string) cell { return cell{kind: cText, b: []byte(s)} }
func num(i int64) cell  { return cell{kind: cInt, i: i} }

// fakeRow is a decoded row: the mapping sees only the parse.Row interface, so its behaviour on any storage
// class in any column is fuzzed here without a database.
type fakeRow struct {
	table string
	cells []cell
}

func (r fakeRow) Table() string      { return r.table }
func (fakeRow) Rowid() (int64, bool) { return 1, true }
func (r fakeRow) IsNull(c int) bool  { return r.cells[c].kind == cNull }
func (r fakeRow) Int(c int) (int64, bool) {
	return r.cells[c].i, r.cells[c].kind == cInt
}

func (r fakeRow) Float(c int) (float64, bool) {
	return r.cells[c].f, r.cells[c].kind == cFloat
}

func (r fakeRow) Text(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cText
}

func (r fakeRow) Blob(c int) ([]byte, bool) {
	return r.cells[c].b, r.cells[c].kind == cBlob
}

func (fakeRow) Range() (records.Range, parse.FileRole) {
	return records.Range{Offset: 0, Length: 10}, "db"
}

func (fakeRow) Locator() (string, bool) { return "row:rowid=1", true }

// joinCols are the columns of the joined tables, by index; the message table's come from messageSpecs.
var joinCols = map[string][]string{
	"handle":                        {"ROWID", "id"},
	"chat":                          {"ROWID", "style", "display_name"},
	"chat_message_join":             {"chat_id", "message_id"},
	"chat_handle_join":              {"chat_id", "handle_id"},
	"attachment":                    {"ROWID", "transfer_name", "mime_type", "total_bytes", "filename"},
	"message_attachment_join":       {"message_id", "attachment_id"},
	"chat_recoverable_message_join": {"chat_id", "message_id", "delete_date"},
}

// joinTables is the order the fuzz input is cut into joined rows.
var joinTables = []string{"handle", "chat", "chat_message_join", "chat_handle_join", "attachment", "message_attachment_join", "chat_recoverable_message_join"}

// fakeCtx answers every join with the rows it was given for the table, whatever the key: the mapping must hold
// for any rows the engine could return.
type fakeCtx struct {
	in   *parse.Input
	rows map[string][]fakeRow
}

func (c fakeCtx) Input() *parse.Input { return c.in }
func (fakeCtx) Note(_, _ string)      {}
func (c fakeCtx) Column(table, col string) int {
	cols := joinCols[table]
	if table == tableMessage {
		cols = names(messageSpecs())
	}
	return slices.Index(cols, col)
}

func (c fakeCtx) Get(table string, _ int64) (parse.Row, bool, error) {
	if rs := c.rows[table]; len(rs) > 0 {
		return rs[0], true, nil
	}
	return nil, false, nil
}

func (c fakeCtx) Match(table, _ string, _ parse.JoinKey) ([]parse.Row, error) {
	var out []parse.Row
	for _, r := range c.rows[table] {
		out = append(out, r)
	}
	return out, nil
}

// world is one fuzzed or hand-built situation: the message row and the rows every join returns.
type world struct {
	msg   []cell
	joins map[string][]fakeRow
}

func (w world) cells() [][]cell {
	out := [][]cell{w.msg}
	for _, t := range joinTables {
		for _, r := range w.joins[t] {
			out = append(out, r.cells)
		}
	}
	return out
}

func (w world) run() ([]records.Record, error) {
	in := &parse.Input{
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/db"}}},
		Budget:    parse.NewBudget(8 << 20).View(),
	}
	return mapMessage(fakeCtx{in: in, rows: w.joins}, fakeRow{table: tableMessage, cells: w.msg})
}

func isNumber(c cell) bool { return c.kind == cInt || c.kind == cFloat }

func msgCol(name string) int { return slices.Index(names(messageSpecs()), name) }

// joinCell is the named cell of the first row the join returns.
func joinCell(w world, table, col string) (cell, bool) {
	rs := w.joins[table]
	if len(rs) == 0 {
		return cell{}, false
	}
	return rs[0].cells[slices.Index(joinCols[table], col)], true
}

// heldTexts is every text any TEXT cell of the world holds, cleaned the way the mapping cleans it, NUL separated.
func (w world) heldTexts() string {
	var sb strings.Builder
	for _, cs := range w.cells() {
		for _, c := range cs {
			if c.kind == cText {
				sb.WriteString(common.CleanText(c.b, maxText).Text)
				sb.WriteByte(0)
			}
		}
	}
	return sb.String()
}

func str(v any) string { s, _ := v.(string); return s }

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func asList(v any) []any { l, _ := v.([]any); return l }

// check holds the mapping to its contract on any world: no panic, and either a typed row error (a counted
// warning) or exactly one record that validates, carries its place, and invents nothing the cells do not say.
func (w world) check(t testing.TB) {
	t.Helper()
	recs, err := w.run()
	if err != nil {
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Fatalf("untyped error %v", err)
		}
		return
	}
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	if r.Type != message.Type || r.ArtifactID != "a1" || r.SourcePath != "/p/db" || r.Locator != "row:rowid=1" || r.Range == nil {
		t.Fatalf("record lost its place: %+v", r)
	}
	if err := message.Validate(r.Payload); err != nil {
		t.Fatalf("payload refused: %v", err)
	}
	for _, k := range []string{"recovery", "snapshot"} {
		if _, has := r.Payload[k]; has {
			t.Errorf("the live mapper states %s", k)
		}
	}
	if r.Recovery != "" {
		t.Errorf("the live mapper sets recovery %q", r.Recovery)
	}
	held := w.heldTexts()
	inHeld := func(where, v string) {
		t.Helper()
		if v != "" && !strings.Contains(held, v) {
			t.Errorf("%s = %q appears in no input cell", where, v)
		}
	}
	at := func(name string) cell { return w.msg[msgCol(name)] }
	p := r.Payload
	raw := asMap(p["raw"])
	unknown := asMap(raw["unknown_columns"])
	flagged := func(name string, want string) {
		t.Helper()
		if unknown[name] != want {
			t.Errorf("%s of kind %d: unknown_columns[%s] = %v, want %q", name, at(name).kind, name, unknown[name], want)
		}
	}

	// direction, channel, status, read: only from the cells that decide them
	fm := at("is_from_me")
	wantDir := message.DirUnknown
	if fm.kind == cInt && fm.i == 1 {
		wantDir = message.DirOut
	} else if fm.kind == cInt && fm.i == 0 {
		wantDir = message.DirIn
	}
	if p["direction"] != wantDir {
		t.Errorf("direction %v from is_from_me %+v", p["direction"], fm)
	}
	if svc := at("service"); svc.kind != cText || !slices.Contains([]string{"iMessage", "SMS", "RCS"}, string(svc.b)) {
		if p["channel"] != "unknown" {
			t.Errorf("channel %v from service %+v", p["channel"], svc)
		}
	}
	if st := p["status"]; (st == message.StatusReceived) != (wantDir == message.DirIn) {
		t.Errorf("status %v with direction %v", st, wantDir)
	}
	if v, has := p["read"]; has {
		if c := at("is_read"); wantDir != message.DirIn || c.kind != cInt || (c.i != 0 && c.i != 1) || v != (c.i == 1) {
			t.Errorf("read %v from is_read %+v with direction %v", v, c, wantDir)
		}
	}

	// text values appear in some cell; a text column holding another storage class is named
	for _, k := range []string{"guid", "subject", "service"} {
		inHeld(k, str(p[k]))
	}
	for _, k := range rawTexts() {
		inHeld("raw."+k, str(raw[k]))
	}
	wantRole := map[string]string{message.DirIn: message.RoleFrom, message.DirOut: message.RoleTo, message.DirUnknown: message.RoleMember}[wantDir]
	for _, e := range asList(p["participants"]) {
		pe := asMap(e)
		inHeld("participant", str(pe["address"]))
		if pe["role"] != wantRole {
			t.Errorf("participant role %v with direction %v", pe["role"], wantDir)
		}
	}
	if th := asMap(p["thread"]); th != nil {
		inHeld("thread.title", str(th["title"]))
		for _, m := range asList(th["members"]) {
			inHeld("thread.members", str(m))
		}
		cmj, ok := joinCell(w, "chat_message_join", "chat_id")
		if !ok || cmj.kind != cInt || th["id"] != strconv.FormatInt(cmj.i, 10) {
			t.Errorf("thread id %v from chat_message_join %+v", th["id"], cmj)
		}
		if g, has := th["group"]; has {
			style, _ := joinCell(w, "chat", "style")
			if style.kind != cInt || (style.i != 43 && style.i != 45) || (g == true) != (style.i == 43) {
				t.Errorf("thread group %v from chat style %+v", g, style)
			}
		}
	}
	for _, e := range asList(p["attachments"]) {
		ae := asMap(e)
		inHeld("attachment name", str(ae["name"]))
		inHeld("attachment mime", str(ae["mime"]))
		inHeld("attachment source_ref", str(ae["source_ref"]))
		if sz, has := ae["size"].(int64); has && sz < 0 {
			t.Errorf("a negative attachment size %d", sz)
		}
		if ae["artifact_id"] != nil {
			t.Errorf("an attachment linked to artifact %v with no lookup in the input", ae["artifact_id"])
		}
	}
	if n := len(asList(p["attachments"])); n > len(w.joins["message_attachment_join"]) {
		t.Errorf("%d attachments from %d join rows", n, len(w.joins["message_attachment_join"]))
	}
	if r.Body != "" {
		switch p["text_source"] {
		case message.TextSourceText:
			if tc := at("text"); tc.kind != cText || r.Body != common.CleanText(tc.b, maxText).Text {
				t.Errorf("body %.30q from text cell %+v", r.Body, tc)
			}
		case message.TextSourceAttributedBody:
			if ab := at("attributedBody"); ab.kind != cBlob {
				t.Errorf("an attributedBody body from cell %+v", ab)
			}
		default:
			t.Errorf("body %.30q with text_source %v", r.Body, p["text_source"])
		}
	}
	for _, col := range []string{"text", "guid", "subject", "service", "cache_roomnames", "group_title", "associated_message_guid", "balloon_bundle_id", "expressive_send_style_id"} {
		if k := at(col).kind; k == cInt || k == cFloat || k == cBlob {
			flagged(col, "type-mismatch")
		}
	}
	for _, col := range rawInts() {
		c := at(col)
		if k := c.kind; k == cFloat || k == cText || k == cBlob {
			flagged(col, "type-mismatch")
		}
		if v, has := raw[col]; has && (c.kind != cInt || v != c.i) {
			t.Errorf("raw[%s] = %v from cell %+v", col, v, c)
		}
	}
	if k := at("attributedBody").kind; k == cInt || k == cFloat || k == cText {
		flagged("attributedBody", "type-mismatch")
	}

	// times: a time needs a numeric cell
	if r.Time != nil && !isNumber(at("date")) {
		t.Errorf("a time %v from date cell %+v", r.Time, at("date"))
	}
	timeCols := map[string]string{"read": "date_read", "delivered": "date_delivered", "played": "date_played", "edited": "date_edited", "retracted": "date_retracted"}
	for _, nt := range r.Times {
		if nt.Kind == "deleted" {
			if dd, ok := joinCell(w, "chat_recoverable_message_join", "delete_date"); !ok || !isNumber(dd) {
				t.Errorf("a deleted time from delete_date %+v", dd)
			}
			continue
		}
		if col, ok := timeCols[nt.Kind]; !ok || !isNumber(at(col)) {
			t.Errorf("a %s time from cell %+v", nt.Kind, at(col))
		}
	}
	for _, col := range []string{"date", "date_read", "date_delivered", "date_played", "date_edited", "date_retracted"} {
		if k := at(col).kind; k == cText || k == cBlob {
			flagged(col, "type-mismatch")
		}
		if raw[col+"_ieee"] != nil && (raw[col] != nil || raw[col+"_status"] != "invalid") {
			t.Errorf("a non-finite %s is stated as a value: %v", col, raw)
		}
	}

	// the recently-deleted marker comes only from its join row
	if r.Deleted && len(w.joins["chat_recoverable_message_join"]) == 0 {
		t.Errorf("deleted with no recoverable join row")
	}
	if r.Deleted != (p["deleted"] != nil) {
		t.Errorf("deleted flag %v and payload.deleted %v disagree", r.Deleted, p["deleted"])
	}
}

// goodWorld is a complete incoming message: every text column holds a value that names it, and each join
// returns the rows that make its handle, chat, member, attachment and recently-deleted marker.
func goodWorld() world {
	var msg []cell
	for _, s := range messageSpecs() {
		switch s.Affinity {
		case "integer":
			msg = append(msg, num(0))
		case "blob":
			msg = append(msg, cell{kind: cNull})
		default:
			msg = append(msg, txt("x-"+s.Name))
		}
	}
	w := world{msg: msg, joins: map[string][]fakeRow{}}
	set := func(col string, c cell) { w.msg[msgCol(col)] = c }
	set("handle_id", num(1))
	set("service", txt("iMessage"))
	set("date", num(704628000))
	set("date_read", num(704628060))
	set("is_read", num(1))
	set("associated_message_guid", cell{kind: cNull})
	row := func(t string, cs ...cell) fakeRow { return fakeRow{table: t, cells: cs} }
	w.joins["handle"] = []fakeRow{row("handle", num(1), txt("+15550001"))}
	w.joins["chat"] = []fakeRow{row("chat", num(1), num(45), txt("x-title"))}
	w.joins["chat_message_join"] = []fakeRow{row("chat_message_join", num(1), num(1))}
	w.joins["chat_handle_join"] = []fakeRow{row("chat_handle_join", num(1), num(1))}
	w.joins["attachment"] = []fakeRow{row("attachment", num(1), txt("a.jpg"), txt("image/jpeg"), num(5), txt("~/Library/SMS/Attachments/ab/a.jpg"))}
	w.joins["message_attachment_join"] = []fakeRow{row("message_attachment_join", num(1), num(1))}
	w.joins["chat_recoverable_message_join"] = []fakeRow{row("chat_recoverable_message_join", num(1), num(1), num(704628100))}
	return w
}

func (w world) clone() world {
	c := world{msg: slices.Clone(w.msg), joins: map[string][]fakeRow{}}
	for t, rs := range w.joins {
		for _, r := range rs {
			c.joins[t] = append(c.joins[t], fakeRow{table: r.table, cells: slices.Clone(r.cells)})
		}
	}
	return c
}

// cellsFrom builds a row of n columns from fuzz bytes: kinds picks each storage class, nums supplies 8 bytes
// a column for the integer and float, texts is split at NUL bytes. off rotates the input so rows differ.
func cellsFrom(n, off int, kinds, nums, texts []byte) []cell {
	var parts [][]byte
	start := 0
	for i, b := range texts {
		if b == 0 {
			parts = append(parts, texts[start:i])
			start = i + 1
		}
	}
	parts = append(parts, texts[start:])
	cs := make([]cell, n)
	for i := range cs {
		j := i + off
		if len(kinds) > 0 {
			cs[i].kind = kinds[j%len(kinds)] % numKinds
		}
		var w [8]byte
		if len(nums) > 0 {
			for k := range w {
				w[k] = nums[(j*8+k)%len(nums)]
			}
		}
		u := binary.LittleEndian.Uint64(w[:])
		cs[i].i, cs[i].f = int64(u), math.Float64frombits(u)
		cs[i].b = parts[j%len(parts)]
	}
	return cs
}

// fuzzWorld builds a world from fuzz bytes: sel gives the number of rows (0 to 3) each join returns, two bits
// a table, and its top bit keeps the integer cells small so the type, style and id paths are reached.
func fuzzWorld(sel uint16, kinds, nums, texts []byte) world {
	w := world{msg: cellsFrom(len(messageSpecs()), 0, kinds, nums, texts), joins: map[string][]fakeRow{}}
	small := sel&0x8000 != 0
	shrink := func(cs []cell) {
		if small {
			for i := range cs {
				if cs[i].kind == cInt {
					cs[i].i = int64(uint64(cs[i].i) % 50)
				}
			}
		}
	}
	shrink(w.msg)
	off := 7
	for ti, t := range joinTables {
		n := int(sel>>(ti*2)) & 3
		for range n {
			cs := cellsFrom(len(joinCols[t]), off, kinds, nums, texts)
			off += 3
			shrink(cs)
			w.joins[t] = append(w.joins[t], fakeRow{table: t, cells: cs})
		}
	}
	return w
}

// The good row maps to exactly what its cells say; then every column of every row, and every row as a whole,
// in every other storage class is contained: a typed row error or one valid record that states nothing the
// cells do not.
func TestMapRowsGoodRowsMapAndHostileCellsAreContained(t *testing.T) {
	w := goodWorld()
	recs, err := w.run()
	if err != nil || len(recs) != 1 {
		t.Fatalf("err %v, records %+v", err, recs)
	}
	r := recs[0]
	p, raw := r.Payload, asMap(r.Payload["raw"])
	if r.Body != "x-text" || p["text_source"] != message.TextSourceText || p["direction"] != message.DirIn || p["status"] != message.StatusReceived ||
		p["channel"] != message.ChannelIMessage || p["kind"] != message.KindText || p["guid"] != "x-guid" || p["subject"] != "x-subject" || p["read"] != true {
		t.Errorf("payload %v body %q", p, r.Body)
	}
	parts := asList(p["participants"])
	if len(parts) != 1 || asMap(parts[0])["address"] != "+15550001" || asMap(parts[0])["role"] != message.RoleFrom || p["participants_unknown"] != nil {
		t.Errorf("participants %v", p["participants"])
	}
	th := asMap(p["thread"])
	if th["id"] != "1" || th["title"] != "x-title" || th["group"] != false || len(asList(th["members"])) != 1 {
		t.Errorf("thread %v", th)
	}
	atts := asList(p["attachments"])
	if len(atts) != 1 || asMap(atts[0])["name"] != "a.jpg" || asMap(atts[0])["mime"] != "image/jpeg" || asMap(atts[0])["size"] != int64(5) ||
		asMap(atts[0])["source_ref"] != "~/Library/SMS/Attachments/ab/a.jpg" || raw["attachments_unresolved"] != int64(1) {
		t.Errorf("attachments %v raw %v", p["attachments"], raw)
	}
	if !r.Deleted || r.Time == nil || len(r.Times) != 2 || raw["recoverable_chat_id"] != int64(1) {
		t.Errorf("deleted %v time %v times %v raw %v", r.Deleted, r.Time, r.Times, raw)
	}
	w.check(t)

	storage := func(k byte) cell { return cell{kind: k, i: 1 << 60, f: math.NaN(), b: []byte{0xff, 0xfe}} }
	for k := range numKinds {
		for col := range w.msg {
			c := w.clone()
			c.msg[col] = storage(k)
			c.check(t)
		}
		for _, tbl := range joinTables {
			for ri := range w.joins[tbl] {
				for col := range w.joins[tbl][ri].cells {
					c := w.clone()
					c.joins[tbl][ri].cells[col] = storage(k)
					c.check(t)
				}
			}
		}
		c := w.clone()
		for i := range c.msg {
			c.msg[i] = storage(k)
		}
		for _, tbl := range joinTables {
			for ri := range c.joins[tbl] {
				for col := range c.joins[tbl][ri].cells {
					c.joins[tbl][ri].cells[col] = storage(k)
				}
			}
		}
		c.check(t)
	}
	for _, v := range []int64{math.MinInt64, -1, 0, 1, 2, 1 << 53, math.MaxInt64, 2001, 3999, 43, 45} {
		c := w.clone()
		for i, cl := range c.msg {
			if cl.kind == cInt {
				c.msg[i].i = v
			}
		}
		for _, tbl := range joinTables {
			for ri := range c.joins[tbl] {
				for col, cl := range c.joins[tbl][ri].cells {
					if cl.kind == cInt {
						c.joins[tbl][ri].cells[col].i = v
					}
				}
			}
		}
		c.check(t)
	}
	for _, f := range []float64{math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1), -3, 1e300, 1e11, 7e17} {
		for _, col := range []string{"date", "date_read", "date_delivered", "date_played", "date_edited", "date_retracted"} {
			c := w.clone()
			c.msg[msgCol(col)] = cell{kind: cFloat, f: f}
			c.check(t)
		}
		c := w.clone()
		c.joins["chat_recoverable_message_join"][0].cells[2] = cell{kind: cFloat, f: f}
		c.check(t)
	}
}

// FuzzMapRows fuzzes the mapping on decoded cells directly: every column of the message and of every joined
// row takes any storage class and value.
func FuzzMapRows(f *testing.F) {
	f.Add(uint16(0xffff), []byte{cText, cInt, cInt, cInt}, []byte{3, 0, 0, 0, 0, 0, 0, 0}, []byte("+1555\x00hi\x00x"))
	f.Add(uint16(0x8555), []byte{cInt, cBlob, cFloat, cNull, cText}, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, []byte("\xff\xfe\x00 12"))
	f.Add(uint16(0), []byte{cBlob, cFloat, cNull}, []byte{}, []byte{})
	f.Add(uint16(0xaaaa), []byte{cText, cText, cText, cInt, cInt, cText, cText, cText, cFloat, cText, cInt, cInt}, []byte{0, 0, 0, 0, 0, 0, 0xf0, 0x7f}, []byte("5\x005\x00x"))
	f.Add(uint16(0x9b9b), []byte{cText}, []byte{1, 0, 0, 0, 0, 0, 0, 0}, []byte("iMessage\x00~/Library/SMS/a\x00+1555"))
	f.Fuzz(func(t *testing.T, sel uint16, kinds, nums, texts []byte) {
		fuzzWorld(sel, kinds, nums, texts).check(t)
	})
}
