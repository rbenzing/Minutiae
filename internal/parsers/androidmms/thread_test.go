package androidmms_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// recipientsDB has one sms whose thread's recipient_ids is the given text.
func recipientsDB(recipients string) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("sms", "create table sms(_id integer primary key, thread_id integer, address text, date integer, type integer, body text)").
		Insert(1, int64(1), int64(1), "+15551234567", int64(1682935200000), int64(1), "hi")
	b.CreateTable("threads", "create table threads(_id integer primary key, recipient_ids text)").Insert(1, int64(1), recipients)
	b.CreateTable("canonical_addresses", "create table canonical_addresses(_id integer primary key, address text)").Insert(1, int64(1), "+15551234567")
	return b.Bytes()
}

func runRecipients(t *testing.T, recipients string, budget int64) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "recip", Files: map[string][]byte{"db": recipientsDB(recipients)}}.Bundle(h, remotePath)
	p := androidmms.Parser{}
	in := b.Input(h, p, false)
	in.Budget = parse.NewBudget(budget).View()
	return h.RunLenient(p, in)
}

// A 3 MB cell of 1.5 million tokens: streamed, the first 1000 resolved, the rest counted.
func TestHostileRecipientCellIsStreamedAndCapped(t *testing.T) {
	res := runRecipients(t, strings.Repeat("1 ", 1_500_000), 32<<20)
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	p := res.Records[0].Payload
	raw := p["raw"].(map[string]any)
	if len(p["thread"].(map[string]any)["members"].([]any)) != 1000 || raw["thread_recipients_total"] != int64(1_500_000) || raw["thread_recipients_truncated"] != true {
		t.Errorf("members %d, raw %v", len(p["thread"].(map[string]any)["members"].([]any)), raw)
	}
}

// A 6 MB cell is over the reader's 4 MiB text cap: its value is not materialized at all,
// so the members are unknown (never a guess from a prefix) and the memory is the reader's cap.
func TestRecipientCellOverTheReaderCapIsUnknown(t *testing.T) {
	res := runRecipients(t, strings.Repeat("1 ", 3_000_000), 32<<20)
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	raw := res.Records[0].Payload["raw"].(map[string]any)
	if raw["thread_state"] != "recipients-unknown" {
		t.Errorf("raw %v, want thread_state recipients-unknown", raw)
	}
	if _, has := res.Records[0].Payload["thread"].(map[string]any)["members"]; has {
		t.Error("members were listed for a cell that was not read")
	}
}
