package androidtelegram_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
)

const factsTSV = "../../../tools/fixtures/schemas/telegram-android/facts.tsv"

// factValues are the values of every facts.tsv row of key (a key may hold one row per pin).
func factValues(t *testing.T, key string) []string {
	t.Helper()
	raw, err := os.ReadFile(factsTSV)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if i == 0 || len(cols) < 2 {
			continue
		}
		if cols[0] == key {
			out = append(out, cols[1])
		}
	}
	return out
}

// 4I4-P4, T6 Q3: a Go constant that follows a pinned fact equals its facts.tsv row (Tasks 10, 11 and 14 add theirs).
func TestTelegramFactsMatchTheCode(t *testing.T) {
	for key, got := range map[string]string{
		"unit.edit_date":  androidtelegram.UnitEditDate,
		"unit.blob_times": androidtelegram.UnitBlobTimes,
		// spec 8.7, T1 c4: the unit of a call duration (Task 11)
		"unit.call_duration": androidtelegram.UnitCallDuration,
		// 4I4-P12, spec 8.6: the enc_chats key of a secret dialog is the low 32 bits cast as pinned (Task 10)
		"cast.encrypted_chat_id": androidtelegram.EncChatIDCast(),
	} {
		vals := factValues(t, key)
		if len(vals) == 0 {
			t.Errorf("facts.tsv has no row %s", key)
		}
		for _, v := range vals {
			if v != got {
				t.Errorf("%s: facts.tsv says %q, the code %q", key, v, got)
			}
		}
	}
	// ruling T2, F30 (Task 14): putReinserts is true when any pinned put of a message table re-inserts: a REPLACE
	// statement deletes the stored row of the key and inserts the new one (an UPDATE rewrites it in place)
	reinserts := false
	for _, key := range []string{"write_verb.messages_v2.put", "write_verb.messages.put"} {
		vals := factValues(t, key)
		if len(vals) == 0 {
			t.Errorf("facts.tsv has no row %s", key)
		}
		reinserts = reinserts || slices.ContainsFunc(vals, func(v string) bool { return strings.HasPrefix(v, "REPLACE ") })
	}
	if androidtelegram.PutReinserts != reinserts {
		t.Errorf("putReinserts = %v, facts.tsv write_verb.*.put says %v", androidtelegram.PutReinserts, reinserts)
	}
}
