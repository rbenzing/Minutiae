package androidmms_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestMMSSpotChecksFromTheSpec(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	one := byLocator(t, res, "sqlite:table=pdu;rowid=1")
	if one.Body != "see photo\nsecond text" || at(t, one.Payload, "text_source") != "part" || at(t, one.Payload, "kind") != "media" ||
		at(t, one.Payload, "raw", "smil_parts") != int64(1) {
		t.Errorf("pdu 1: body %q payload %v", one.Body, one.Payload)
	}
	// Time comes from SECONDS (unlike sms, which is milliseconds)
	if one.Time == nil || one.Time.T.Unix() != 1682935200+300 {
		t.Errorf("pdu 1 time %v", one.Time)
	}
	if one.SourcePath != remotePath {
		t.Errorf("pdu 1 SourcePath %q, want %q", one.SourcePath, remotePath)
	}
	// the one attachment is in the case: identity from Lookup, never from the bytes of the part
	if at(t, one.Payload, "attachments", "0", "name") != "img.jpg" || at(t, one.Payload, "attachments", "0", "mime") != "image/jpeg" ||
		at(t, one.Payload, "attachments", "0", "artifact_id") != b.ByPath[remoteAttachment].ID ||
		at(t, one.Payload, "attachments", "0", "artifact_sha256") != b.ByPath[remoteAttachment].SHA256 {
		t.Errorf("pdu 1 attachment: %v", one.Payload["attachments"])
	}
	group := byLocator(t, res, "sqlite:table=pdu;rowid=2")
	roles := []string{}
	for i := 0; i < 6; i++ {
		roles = append(roles, at(t, group.Payload, "participants", string(rune('0'+i)), "role").(string))
	}
	if !slices.Equal(roles, []string{"from", "to", "to", "cc", "bcc", "member"}) || at(t, group.Payload, "direction") != "out" {
		t.Errorf("group MMS participants %v", roles)
	}
	notification := byLocator(t, res, "sqlite:table=pdu;rowid=3")
	if at(t, notification.Payload, "kind") != "system" {
		t.Errorf("m_type 130 is a notification, kind %v", notification.Payload["kind"])
	}
	bare := byLocator(t, res, "sqlite:table=pdu;rowid=4")
	if at(t, bare.Payload, "participants_unknown") != true || at(t, bare.Payload, "kind") != "unknown" {
		t.Errorf("pdu 4 has no addr and no part rows: %v", bare.Payload)
	}
	// a path containing * cannot be written as a glob: it is not looked up, and not linked
	draft := byLocator(t, res, "sqlite:table=pdu;rowid=5")
	atts, _ := draft.Payload["attachments"].([]any)
	if len(atts) == 0 {
		t.Fatalf("pdu 5 has no attachment: %v", draft.Payload)
	}
	if _, linked := atts[0].(map[string]any)["artifact_id"]; linked {
		t.Error("an attachment whose path contains * was linked")
	}
	if at(t, draft.Payload, "raw", "text_parts_without_text") != int64(1) || draft.Body != "" {
		t.Errorf("pdu 5: %v", draft.Payload)
	}
	zero := byLocator(t, res, "sqlite:table=pdu;rowid=6")
	if zero.Time != nil || at(t, zero.Payload, "raw", "date_status") != "sentinel" {
		t.Errorf("pdu 6: time %v raw %v", zero.Time, zero.Payload["raw"])
	}
}

const remoteAttachment = "/data/user/0/com.android.providers.telephony/app_parts/PART_1.jpg"

// lossyMMSDB has 300 pdu rows whose single text part is part row N (mid = N); the second
// leaf of the part (or addr) table has its cell count zeroed, so no scan delivers its rows.
func lossyMMSDB(t *testing.T, damage string) (data []byte, lost map[int64]bool) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	b.CreateTable("sms", "create table sms(_id integer primary key, address text, date integer, type integer, body text)") // required, empty
	pdu := b.CreateTable("pdu", "create table pdu(_id integer primary key, date integer, msg_box integer, m_type integer)")
	addr := b.CreateTable("addr", "create table addr(_id integer primary key, msg_id integer, address text, type integer)")
	part := b.CreateTable("part", "create table part(_id integer primary key, mid integer, seq integer, ct text, text text)")
	for i := int64(1); i <= 300; i++ {
		pdu.Insert(i, i, int64(1682935200)+i, int64(1), int64(132))
		addr.Insert(i, i, i, "+1555000"+string(rune('0'+i%10)), int64(137))
		part.Insert(i, i, i, int64(0), "text/plain", "text of mms")
	}
	data = b.Bytes()
	target := part
	if damage == "addr" {
		target = addr
	}
	leaves := target.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := target.CellBytes(i); pg == leaves[1] {
			lost[i] = true
		}
	}
	page := data[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0
	if len(lost) == 0 {
		t.Fatal("the damaged leaf holds no rows")
	}
	return data, lost
}

func runDB(t *testing.T, data []byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "lossy", Files: map[string][]byte{"db": data}}.Bundle(h, remotePath)
	p := androidmms.Parser{}
	return h.RunLenient(p, b.Input(h, p, false))
}

func recordsOfPDU(t *testing.T, res parsertest.Result) map[int64]records.Record {
	t.Helper()
	out := map[int64]records.Record{}
	for _, r := range res.Records {
		var id int64
		if _, err := scanRowid(r.Locator, &id); err == nil {
			out[id] = r
		}
	}
	return out
}

func scanRowid(loc string, id *int64) (int, error) {
	return fmt.Sscanf(loc, "sqlite:table=pdu;rowid=%d", id)
}

// Review Focus 1: an undecidable join is unknown, never "this message has no text".
// Ruling T19: a file with the same path acquired from ANOTHER device is not this message's attachment.
func TestAttachmentFromAnotherDeviceIsNeverLinked(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	for i := range fx.Expect.ExtraFiles {
		fx.Expect.ExtraFiles[i].Device = "SER2"
	}
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	one := byLocator(t, res, "sqlite:table=pdu;rowid=1")
	att := at(t, one.Payload, "attachments", "0").(map[string]any)
	if _, linked := att["artifact_id"]; linked {
		t.Error("an attachment was linked to a file from another device")
	}
	if at(t, one.Payload, "raw", "attachments_other_source") != int64(1) {
		t.Errorf("raw %v: the other-device match must be counted", one.Payload["raw"])
	}
}

// With the same path on two devices only the message's own device's file is the link.
func TestAttachmentLinksTheFileOfTheMessagesOwnDeviceOnly(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	other := fx.Expect.ExtraFiles[0]
	other.Device, other.ContentB64 = "SER2", "b3RoZXIgZGV2aWNlIGJ5dGVz"
	fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles, other)
	b := fx.Bundle(h, remotePath)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	att := at(t, byLocator(t, res, "sqlite:table=pdu;rowid=1").Payload, "attachments", "0").(map[string]any)
	if att["artifact_id"] != b.Others["extra0"].ID {
		t.Errorf("linked %v, want the SER1 file %s", att["artifact_id"], b.Others["extra0"].ID)
	}
}

// Rulings T25(b) and T46: two acquisitions of ONE device never cross-link. The attachment exists only in a
// later acquisition of the same phone, so the message of the first is not its owner.
func TestAttachmentFromAnotherAcquisitionOfTheSameDeviceIsNeverLinked(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	data := []byte("pulled in the second acquisition")
	fx.Expect.ExtraFiles = nil
	b := fx.Bundle(h, remotePath)
	later := h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq2", remoteAttachment, data), data)
	b.Others["extra0"] = later
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	one := byLocator(t, res, "sqlite:table=pdu;rowid=1")
	att := at(t, one.Payload, "attachments", "0").(map[string]any)
	if _, linked := att["artifact_id"]; linked {
		t.Error("an attachment was linked to a file of another acquisition of the device")
	}
	if at(t, one.Payload, "raw", "attachments_other_source") != int64(1) {
		t.Errorf("raw %v: the other-acquisition match must be counted", one.Payload["raw"])
	}
}

// With the same path in two acquisitions of one device only the message's own acquisition's file is the link.
func TestAttachmentLinksTheFileOfTheMessagesOwnAcquisitionOnly(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
	b := fx.Bundle(h, remotePath)
	data := []byte("pulled in the second acquisition")
	b.Others["extra1"] = h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq2", remoteAttachment, data), data)
	p := androidmms.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	att := at(t, byLocator(t, res, "sqlite:table=pdu;rowid=1").Payload, "attachments", "0").(map[string]any)
	if att["artifact_id"] != b.Others["extra0"].ID {
		t.Errorf("linked %v, want the file of the database's own acquisition %s", att["artifact_id"], b.Others["extra0"].ID)
	}
}

// Ruling T46: an empty acquisition is unknown. It never links, on either side.
func TestAnEmptyAcquisitionNeverLinksAnAttachment(t *testing.T) {
	for name, strip := range map[string]func(b *parsertest.Bundle){
		"attachment unknown": func(b *parsertest.Bundle) {
			a := b.Others["extra0"]
			a.Acquisition = ""
			b.Others["extra0"] = a
		},
		"database unknown": func(b *parsertest.Bundle) { b.Primary.Acquisition = "" },
		"both unknown": func(b *parsertest.Bundle) {
			b.Primary.Acquisition = ""
			a := b.Others["extra0"]
			a.Acquisition = ""
			b.Others["extra0"] = a
		},
	} {
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", "aosp-baseline")
		b := fx.Bundle(h, remotePath)
		if b.Others["extra0"].Acquisition == "" || b.Primary.Acquisition == "" {
			t.Fatalf("%s: the harness leaves the acquisition empty, so this test proves nothing", name)
		}
		strip(&b)
		p := androidmms.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		att := at(t, byLocator(t, res, "sqlite:table=pdu;rowid=1").Payload, "attachments", "0").(map[string]any)
		if _, linked := att["artifact_id"]; linked {
			t.Errorf("%s: an attachment was linked although an acquisition is unknown", name)
		}
	}
}

func TestMMSPartsJoinUndecidedIsUnknownNotEmpty(t *testing.T) {
	data, lost := lossyMMSDB(t, "part")
	res := runDB(t, data)
	got := recordsOfPDU(t, res)
	if len(got) != 300 {
		t.Fatalf("%d records, want all 300 pdu rows (a lost part must not drop the message)", len(got))
	}
	for id, r := range got {
		if !slices.Equal(r.Payload["raw"].(map[string]any)["join_undecided"].([]any), []any{"part"}) {
			t.Errorf("pdu %d: join_undecided = %v; every answer of a lossy index is flagged", id, r.Payload["raw"])
		}
		switch {
		case lost[id] && (r.Body != "" || r.Payload["text_source"] != nil):
			t.Errorf("pdu %d invented a body %q for a part it could not read", id, r.Body)
		case !lost[id] && r.Body != "text of mms":
			t.Errorf("pdu %d lost the body it could read: %q", id, r.Body)
		}
	}
}

func TestMMSAddrJoinUndecidedIsUnknownNotEmpty(t *testing.T) {
	data, lost := lossyMMSDB(t, "addr")
	got := recordsOfPDU(t, runDB(t, data))
	if len(got) != 300 {
		t.Fatalf("%d records, want 300", len(got))
	}
	for id, r := range got {
		raw := r.Payload["raw"].(map[string]any)
		if !slices.Equal(raw["join_undecided"].([]any), []any{"addr"}) {
			t.Errorf("pdu %d: join_undecided = %v", id, raw["join_undecided"])
		}
		if lost[id] && r.Payload["participants_unknown"] != true {
			t.Errorf("pdu %d: participants of a lost addr row are not marked unknown", id)
		}
		if !lost[id] && r.Payload["participants_unknown"] == true {
			t.Errorf("pdu %d: a readable address was dropped", id)
		}
	}
}
