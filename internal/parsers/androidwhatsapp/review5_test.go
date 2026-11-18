package androidwhatsapp_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The review of plan 4I (fix 5), I1: the lookup of a media path uses the stored path, never its cleaned display text. A
// path whose cleaning changed anything (NUL, invalid UTF-8) is not looked up: raw.media_state and a counted undecided
// lookup, so a case file at the literal U+FFFD path is never linked to a stored path that names no such file.
func TestWhatsAppMediaPathThatCleaningChangedIsNeverLooked(t *testing.T) {
	const dir = "Media/WhatsApp Images/"
	const caseDir = "/storage/emulated/0/WhatsApp/" + dir
	content := base64.StdEncoding.EncodeToString([]byte("jpeg bytes"))
	for _, c := range []struct {
		name, stored, candidate string
		linked                  bool
	}{
		{"control, a clean name is linked", "a.jpg", "a.jpg", true},
		{"NUL", "a\x00b.jpg", "a�b.jpg", false},
		{"invalid UTF-8", "a\xffb.jpg", "a�b.jpg", false},
	} {
		db, _ := buildDB(nil, func(tb tables) {
			jidRow(tb, 1, "15550000001", "s.whatsapp.net", "15550000001@s.whatsapp.net")
			tb["chat"].Insert(1, nil, int64(1), nil)
			tb["message"].Insert(1, nil, int64(1), int64(0), "K1", nil, int64(0), goodTS, nil, int64(1), nil)
			tb["message_media"].Insert(1, nil, dir+c.stored, int64(10), "image/jpeg", "x.jpg")
		})
		fx := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db}}
		fx.Expect.ExtraFiles = append(fx.Expect.ExtraFiles, parsertest.ExtraFile{RemotePath: caseDir + c.candidate, ContentB64: content})
		res := parseFixture(t, fx, remoteLive)
		r := byRowid(t, res)["message/1"]
		raw := rawMap(t, r)
		if got := linked(t, r) != ""; got != c.linked {
			t.Errorf("%s: linked %v, want %v (raw %v)", c.name, got, c.linked, raw)
		}
		if c.linked {
			continue
		}
		if raw["media_state"] != "lookup-path-not-expressible" || fmt.Sprint(raw["attachments_lookup_undecided"]) != "1" ||
			fmt.Sprint(raw["attachments_unresolved"]) != "0" || fmt.Sprint(raw["attachments_resolved"]) != "0" {
			t.Errorf("%s: raw %v, want media_state lookup-path-not-expressible counted undecided", c.name, raw)
		}
		if a := attachmentsOf(t, r); len(a) != 1 || a[0]["source_ref"] != dir+c.candidate {
			t.Errorf("%s: attachments %v, want the cleaned display text as source_ref", c.name, a)
		}
		if fl, _ := raw["text_flags"].([]any); len(fl) == 0 {
			t.Errorf("%s: raw %v, want text_flags", c.name, raw)
		}
	}
}
