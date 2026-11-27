package androidtelegram

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/tl"
)

// Task 1 review (carried into plan 4I4c Task 4; unknown is never absent): a media object whose (type, ID) the table
// lacks still states raw.media with the constructor read, and nothing else of it.
func TestMediaSummaryStatesAnIDWithoutEntry(t *testing.T) {
	tab := newTable()
	msg := tab[tl.Key{Type: typeMessage, ID: idOf(t, tab, "TL_message")}]
	v := newView(tl.Result{Fields: []tl.Named{{Path: "media", Kind: tl.KObject, ID: 0x3ded6320, Type: "NotAType"}}})
	v.tab = tab
	raw := map[string]any{}
	if atts := mediaSummary(v, tab, msg, raw); atts != nil {
		t.Errorf("attachments %v, want none", atts)
	}
	m, ok := raw["media"].(map[string]any)
	if !ok || len(m) != 1 || m["constructor"] != hexID(0x3ded6320) {
		t.Errorf("raw.media = %v, want only the constructor 0x3ded6320", raw["media"])
	}
}

// Plan 4I4c W3 (design section 6): a string vector's element count is a vector length like any other.
func TestViewLenCountsAStringVector(t *testing.T) {
	v := newView(tl.Result{Fields: []tl.Named{{Path: "v", Kind: tl.KVecString, Len: 3}}})
	if n, ok := v.Len("v"); !ok || n != 3 {
		t.Errorf("Len of a string vector = %d, %v; want 3", n, ok)
	}
}
