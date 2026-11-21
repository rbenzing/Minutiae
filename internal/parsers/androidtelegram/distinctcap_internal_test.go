package androidtelegram

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// msgRecord is a message record of table messages_v2 with the given constructor and secret-chat key.
func msgRecord(i int, ctor string, chat int64) records.Record {
	return records.Record{
		Locator: "sqlite:table=messages_v2;rowid=" + strconv.Itoa(i),
		Payload: map[string]any{"raw": map[string]any{"data_constructor": ctor, "enc_chat_id": chat, "secret_chat": true}},
	}
}

// 4I4 final review M3: the distinct constructor ids and secret-chat keys the counting emitter tracks are bounded by
// maxDistinctKeys (outside MemBudget); overflow folds into other, and the notes say so.
func TestCountingEmitterBoundsDistinctKeys(t *testing.T) {
	if maxDistinctKeys != 4096 {
		t.Fatalf("maxDistinctKeys = %d, want 4096", maxDistinctKeys)
	}
	ce := newCountingEmitter(&recEmitter{notes: map[string]string{}})
	ce.maxKeys = 3
	for i := 0; i < 5; i++ { // five distinct constructors and chats; the first three are tracked
		if err := ce.Emit(context.Background(), msgRecord(i, "0x0000000"+strconv.Itoa(i), int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	// a tracked key still counts after the cap
	if err := ce.Emit(context.Background(), msgRecord(9, "0x00000000", 1)); err != nil {
		t.Fatal(err)
	}
	if len(ce.seen) != 3 || len(ce.secret) != 3 {
		t.Fatalf("tracked %d constructors and %d chats, want 3 and 3", len(ce.seen), len(ce.secret))
	}
	out := &recEmitter{notes: map[string]string{}}
	writeCountNotes(out, ce)
	if got := out.notes["constructors_seen"]; !strings.Contains(got, "0x00000000:2") || !strings.Contains(got, "other:2") {
		t.Errorf("constructors_seen %q: want the tracked top id and other:2 for the folded records", got)
	}
	if got := out.notes["constructors_seen_capped"]; got != "3" {
		t.Errorf("constructors_seen_capped %q, want the cap 3", got)
	}
	if got := out.notes["secret_chats"]; !strings.HasPrefix(got, ">=3") || !strings.Contains(got, "capped at 3") {
		t.Errorf("secret_chats %q: want a stated lower bound >=3 naming the cap", got)
	}
}

func TestCountingEmitterUncappedNotesAreUnchanged(t *testing.T) {
	ce := newCountingEmitter(&recEmitter{notes: map[string]string{}})
	_ = ce.Emit(context.Background(), msgRecord(1, "0x00000001", 7))
	out := &recEmitter{notes: map[string]string{}}
	writeCountNotes(out, ce)
	if _, has := out.notes["constructors_seen_capped"]; has || out.notes["secret_chats"] != "1" {
		t.Errorf("notes %v: an uncapped run must not mention the cap", out.notes)
	}
	var _ parse.Emitter = out
}
