package ioswhatsapp_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// unknownOf is raw.unknown_columns of a record.
func unknownOf(r records.Record) map[string]any {
	m, _ := rawOf(r)["unknown_columns"].(map[string]any)
	return m
}

// Final review I1: a stored REAL NaN, which the reader gives as NULL with its serial type 7, in a key, code or text column
// is a value of another class: undecided and listed in unknown_columns, never "absent", "no row" or an empty body.
func TestIOSWhatsAppChatsNaNInAKeyCodeOrTextColumnIsUndecided(t *testing.T) {
	nan := math.NaN()
	cases := map[string]int64{}
	db, _ := buildChat(nil, func(tb chatTables) {
		s := tb["ZWACHATSESSION"]
		s.put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		s.put(2, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": nan})
		s.put(3, map[string]any{"ZSESSIONTYPE": int64(1), "ZCONTACTJID": grp})
		tb["ZWAGROUPMEMBER"].put(1, map[string]any{"ZMEMBERJID": nan})
		m := tb["ZWAMESSAGE"]
		pk := int64(0)
		add := func(name string, kv map[string]any) {
			pk++
			cases[name] = pk
			m.put(pk, msgRow(kv))
		}
		add("chat key", map[string]any{"ZCHATSESSION": nan})
		add("chat jid", map[string]any{"ZCHATSESSION": int64(2)})
		add("media key", map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil, "ZMEDIAITEM": nan})
		add("from me", map[string]any{"ZISFROMME": nan})
		add("text", map[string]any{"ZTEXT": nan})
		add("group member key", map[string]any{"ZCHATSESSION": int64(3), "ZGROUPMEMBER": nan})
		add("member jid", map[string]any{"ZCHATSESSION": int64(3), "ZGROUPMEMBER": int64(1)})
		add("receipt key", map[string]any{"ZMESSAGEINFO": nan})
		add("local path", map[string]any{"ZMESSAGETYPE": int64(1), "ZTEXT": nil})
		tb["ZWAMEDIAITEM"].put(1, map[string]any{"ZMESSAGE": cases["local path"], "ZMEDIALOCALPATH": nan, "ZVCARDSTRING": "image/jpeg"})
	})
	recs, _ := runChat(t, db, nil, "whatsapp-ios-join-undecided")
	get := func(name string) records.Record {
		t.Helper()
		r, ok := recs[cases[name]]
		if !ok {
			t.Fatalf("%s: no record", name)
		}
		return r
	}
	flagged := func(name, col string) {
		t.Helper()
		if got := unknownOf(get(name))[col]; got != "type-mismatch" {
			t.Errorf("%s: unknown_columns[%s] = %v, want type-mismatch (raw %v)", name, col, got, rawOf(get(name)))
		}
	}
	state := func(name, key, want string) {
		t.Helper()
		if got := rawOf(get(name))[key]; got != want {
			t.Errorf("%s: raw.%s = %v, want %q (raw %v)", name, key, got, want, rawOf(get(name)))
		}
	}
	state("chat key", "chat_state", "undecided")
	flagged("chat key", "ZWAMESSAGE.ZCHATSESSION")
	state("chat jid", "chat_state", "undecided")
	flagged("chat jid", "ZWACHATSESSION.ZCONTACTJID")
	if got := rawOf(get("media key"))["media_state"]; got == "no-ZWAMEDIAITEM-row" {
		t.Errorf("media key: a NaN key is read as no row: %v", rawOf(get("media key")))
	}
	if got := rawOf(get("media key"))["join_undecided"]; !reflect.DeepEqual(got, []any{"ZWAMEDIAITEM"}) {
		t.Errorf("media key: join_undecided = %v", got)
	}
	flagged("media key", "ZWAMESSAGE.ZMEDIAITEM")
	flagged("from me", "ZWAMESSAGE.ZISFROMME")
	flagged("text", "ZWAMESSAGE.ZTEXT")
	state("group member key", "sender_state", "undecided")
	flagged("group member key", "ZWAMESSAGE.ZGROUPMEMBER")
	state("member jid", "sender_state", "undecided")
	flagged("member jid", "ZWAGROUPMEMBER.ZMEMBERJID")
	flagged("receipt key", "ZWAMESSAGE.ZMESSAGEINFO")
	state("local path", "media_state", "local-path-unreadable")
	flagged("local path", "ZWAMEDIAITEM.ZMEDIALOCALPATH")
}

// mismatchStore is a ChatStorage whose Z_PRIMARYKEY has extra entity rows and whose ZWAMESSAGE has extra message rows, each
// with one byte more than its header accounts for: the reader delivers them and warns (record-length-mismatch). The model read
// of preflight raises the first warnings, rowrun.Run's scan of the messages the others.
func mismatchStore(models, messages int) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	m := b.CreateTable("ZWAMESSAGE", "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZMESSAGETYPE INTEGER)")
	c := b.CreateTable("ZWACHATSESSION", "CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR)")
	for i := 1; i <= messages; i++ {
		m.InsertRaw(int64(i), []uint64{0, 1, 1, 1, 1}, []byte{1, 0, 5, 0, 'x'})
	}
	c.Insert(1, nil, p1)
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	z.Insert(1, nil, "WAMessage", int64(0), int64(0))
	z.Insert(2, nil, "WAChatSession", int64(0), int64(0))
	for i := 0; i < models; i++ {
		name := fmt.Sprintf("E%05d", i)
		z.InsertRaw(int64(i+3), []uint64{0, uint64(2*len(name) + 13), 8, 8}, []byte(name+"x"))
	}
	return b.Bytes()
}

// refusing is an emitter that refuses every reader warning and drops everything else.
type refusing struct{}

func (refusing) Emit(context.Context, records.Record) error { return nil }
func (refusing) Warn(_ context.Context, locator, _ string) error {
	if strings.HasPrefix(locator, "sqlite:file=") {
		return errors.New("warning refused")
	}
	return nil
}
func (refusing) Note(string, string)   {}
func (refusing) Progress(int64, int64) {}

// Final review M2: a reader warning of preflight that the host refuses is the job's failure, never swallowed.
func TestIOSWhatsAppChatsARefusedPreflightWarningFailsTheJob(t *testing.T) {
	h := parsertest.NewHarness(t)
	bd := parsertest.Fixture{Name: "refuse", Files: map[string][]byte{"db": mismatchStore(1, 0)}}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	if err := p.Parse(t.Context(), bd.Input(h, p, false), refusing{}); err == nil || !strings.Contains(err.Error(), "warning refused") {
		t.Errorf("err %v, want the refused warning", err)
	}
}

// Final review M2: preflight and rowrun.Run each state at most 1000 reader warnings; the job states one warnings_dropped,
// the total of both, written after Run (a note's first write wins, so a preflight note would mask Run's). Each of the two
// opens of this store lists 1000 warnings and the reader's "further warnings suppressed": each drops one.
func TestIOSWhatsAppChatsWarningsDroppedIsOneTotal(t *testing.T) {
	h := parsertest.NewHarness(t)
	bd := parsertest.Fixture{Name: "dropped", Files: map[string][]byte{"db": mismatchStore(1002, 1003)}}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, bd.Input(h, p, false), "record-length-mismatch")
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	if got := res.Notes["warnings_dropped"]; got != "2" || len(res.Warnings) != 2000 {
		t.Errorf("warnings_dropped = %q with %d warnings, want 2 (one of preflight, one of Run) with 2000", got, len(res.Warnings))
	}
}

// Final review M1: the label of ZGROUPEVENTTYPE is read only for a type-6 row. On a 1:1 text row (type 0, session type 0)
// a stored group event code is kept in raw as stored and gets no label; the kind, the body and the participants are those of
// an ordinary message.
func TestIOSWhatsAppChatsGroupEventCodeOnAnOrdinaryRowIsRawOnly(t *testing.T) {
	db, _ := buildChat(nil, func(tb chatTables) {
		tb["ZWACHATSESSION"].put(1, map[string]any{"ZSESSIONTYPE": int64(0), "ZCONTACTJID": p1})
		tb["ZWAMESSAGE"].put(1, msgRow(map[string]any{"ZGROUPEVENTTYPE": int64(2)}))
		tb["ZWAMESSAGE"].put(2, msgRow(nil))
	})
	recs, _ := runChat(t, db, nil)
	r, ctl := recs[1], recs[2]
	raw := rawOf(r)
	if raw["ZGROUPEVENTTYPE"] != int64(2) || raw["label_ZGROUPEVENTTYPE"] != nil {
		t.Errorf("raw %v, want ZGROUPEVENTTYPE 2 and no label", raw)
	}
	if r.Payload["kind"] != "text" || r.Body != "t" || fromOf(r, "from") != p1 || unknownParticipants(r) {
		t.Errorf("payload %v, want an ordinary text message from %s", r.Payload, p1)
	}
	if !reflect.DeepEqual(r.Payload["participants"], ctl.Payload["participants"]) || r.Payload["kind"] != ctl.Payload["kind"] {
		t.Errorf("participants %v kind %v, want those of the control row %v %v", r.Payload["participants"], r.Payload["kind"], ctl.Payload["participants"], ctl.Payload["kind"])
	}
}
