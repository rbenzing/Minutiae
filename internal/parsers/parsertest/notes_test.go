package parsertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/parse"
)

// noteParser writes the notes its test asks for, then one valid record.
type noteParser struct {
	WellBehaved
	notes func(out parse.Emitter)
}

func (p noteParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	p.notes(out)
	return out.Emit(ctx, validRecord(in, 1))
}

// The harness stores notes by the host's rules: the first value of a key wins, at most MaxNotes
// distinct keys, key and value cut and cleaned. A parser that passes the harness cannot be limited
// differently by the host.
func TestHarnessNotesFollowTheHostRules(t *testing.T) {
	h, art, _ := bundle(t, t, "nt", "one\n")
	run := func(notes func(parse.Emitter)) Result {
		p := noteParser{WellBehaved{Name: "nt", Version: "1.0.0"}, notes}
		return h.RunLenient(p, h.Input(p, art, nil, false))
	}

	res := run(func(out parse.Emitter) {
		out.Note("k", "first")
		out.Note("k", "second")
	})
	if res.Notes["k"] != "first" {
		t.Errorf("duplicate key: %q, want the first value", res.Notes["k"])
	}

	res = run(func(out parse.Emitter) {
		for i := range 500 {
			out.Note(fmt.Sprintf("key%03d", i), "v")
		}
	})
	if want := parse.DefaultLimits().MaxNotes; len(res.Notes) != want {
		t.Errorf("%d notes kept, want %d (Limits.MaxNotes)", len(res.Notes), want)
	}
	if _, ok := res.Notes["key499"]; ok {
		t.Error("a note past the cap was kept")
	}

	res = run(func(out parse.Emitter) {
		out.Note(strings.Repeat("k", 200), strings.Repeat("v", 5000))
		out.Note("bad", "a\xffb\x00c")
	})
	for k, v := range res.Notes {
		if k == "bad" {
			if v != "a?b?c" || !utf8.ValidString(v) {
				t.Errorf("invalid text kept as %q, want a?b?c", v)
			}
			continue
		}
		if len(k) != 64 || !strings.HasSuffix(k, "...") || len(v) != 1<<10 || !strings.HasSuffix(v, "...") {
			t.Errorf("long note kept as key %d bytes, value %d bytes", len(k), len(v))
		}
	}
}
