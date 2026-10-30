package iossms

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func stream(text string) []byte {
	b := append([]byte{0x04, 0x0B}, "streamtyped"...)
	b = append(b, 0x81, 0xE8, 0x03, 0x84, 0x84, 0x84, 8)
	b = append(b, "NSString"...)
	b = append(b, 0x01, 0x84, 0x01, '+', byte(len(text)))
	return append(b, text...)
}

// Review fix 4: the host budget refusing the extraction is "budget", a fact about the run, not the data.
func TestDecodeBodyStatuses(t *testing.T) {
	big := parse.NewBudget(1 << 20).View()
	if s, st := decodeBody(stream("hello"), big); s != "hello" || st != "" {
		t.Errorf("good stream: %q %q", s, st)
	}
	if _, st := decodeBody(stream("hello"), parse.NewBudget(1).View()); st != "budget" {
		t.Errorf("exhausted budget: status %q", st)
	}
	if _, st := decodeBody(stream("hello"), nil); st != "budget" {
		t.Errorf("no budget: status %q", st)
	}
	if _, st := decodeBody([]byte("not a stream"), big); st != "undecodable" {
		t.Errorf("garbage: status %q", st)
	}
	if _, st := decodeBody(stream("hello")[:20], big); st != "undecodable" {
		t.Errorf("cut stream: status %q", st)
	}
	long := append(stream("")[:14], make([]byte, 70000)...)
	if _, st := decodeBody(long, big); st != "no-string" {
		t.Errorf("no string object: status %q", st)
	}
}

type fakeEmitter struct {
	parse.Emitter
	err error
	n   int
}

func (f *fakeEmitter) Emit(context.Context, records.Record) error { f.n++; return f.err }

func differing(raw any) records.Record {
	return records.Record{Payload: map[string]any{"raw": raw}}
}

// Review fix 6: the count follows a successful Emit and a payload of an unexpected shape is an error.
func TestCountingEmitter(t *testing.T) {
	inner := &fakeEmitter{}
	c := &counting{Emitter: inner}
	if err := c.Emit(t.Context(), differing(map[string]any{"attributed_body_differs": true})); err != nil || c.differs != 1 {
		t.Errorf("counted record: err %v differs %d", err, c.differs)
	}
	if err := c.Emit(t.Context(), differing(map[string]any{"attributed_body_differs": false})); err != nil || c.differs != 1 {
		t.Errorf("record without the flag: err %v differs %d", err, c.differs)
	}
	boom := errors.New("emit failed")
	inner.err = boom
	if err := c.Emit(t.Context(), differing(map[string]any{"attributed_body_differs": true})); !errors.Is(err, boom) || c.differs != 1 {
		t.Errorf("a failed Emit was counted: err %v differs %d", err, c.differs)
	}
	inner.err = nil
	n := inner.n
	if err := c.Emit(t.Context(), differing("not a map")); err == nil || inner.n != n && c.differs != 1 {
		t.Errorf("an unexpected raw type must fail closed: err %v", err)
	}
	if err := c.Emit(t.Context(), records.Record{Payload: map[string]any{}}); err == nil {
		t.Error("a payload without raw must fail closed")
	}
}
