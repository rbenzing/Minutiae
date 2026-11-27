package androidtelegram

import (
	"strings"
	"testing"
)

// Spec 12 (C4): an identifier is never cut. Within maxIdent it is kept (cleaned); over it, or once its invalid bytes
// grow past it as U+FFFD, it is absent with its state, its stored length and its SHA-256.
func TestTelegramIdentIsNeverCut(t *testing.T) {
	raw := map[string]any{}
	if s, ok := ident(raw, "username", []byte("ann_example"), maxIdent); !ok || s != "ann_example" || len(raw) != 0 {
		t.Errorf("a short identifier: %q %v %v", s, ok, raw)
	}
	long := []byte(strings.Repeat("a", maxIdent+1))
	if s, ok := ident(raw, "username", long, maxIdent); ok || s != "" {
		t.Errorf("an identifier over the limit was kept: %q", s)
	}
	if raw["username_state"] != "too-long" || raw["username_len"] != int64(maxIdent+1) || raw["username_sha256"] != sha256Hex(long) {
		t.Errorf("raw %v", raw)
	}
	grows := append([]byte(strings.Repeat("a", maxIdent-1)), 0xff) // one invalid byte becomes 3 bytes of U+FFFD
	raw = map[string]any{}
	if _, ok := ident(raw, "phone", grows, maxIdent); ok || raw["phone_state"] != "too-long" {
		t.Errorf("an identifier grown past the limit by U+FFFD was kept: %v", raw)
	}
}

// identClean is the cleaned text of an identifier and whether its cleaning flagged anything; it refuses a stored text
// over the limit before any copy and one the cleaning grows past it (the shared core of ident and the poll's countries).
func TestTelegramIdentCleanShared(t *testing.T) {
	if s, flagged, ok := identClean([]byte("FR"), maxTiny); !ok || s != "FR" || flagged {
		t.Errorf("a clean code: %q %v %v", s, flagged, ok)
	}
	if s, flagged, ok := identClean([]byte{'F', 0xff}, maxTiny); !ok || s != "F�" || !flagged {
		t.Errorf("an invalid byte: %q %v %v; want it cleaned and flagged", s, flagged, ok)
	}
	if _, _, ok := identClean([]byte(strings.Repeat("a", maxTiny+1)), maxTiny); ok {
		t.Error("a stored text over the limit was kept")
	}
	if _, _, ok := identClean(append([]byte(strings.Repeat("a", maxTiny-1)), 0xff), maxTiny); ok {
		t.Error("a text grown past the limit by U+FFFD was kept")
	}
}
