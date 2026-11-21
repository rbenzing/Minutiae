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
