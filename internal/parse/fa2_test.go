package parse

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestWithoutDeadlineHidesOnlyTheDeadline(t *testing.T) {
	type key struct{}
	base, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "v"), time.Hour)
	defer cancel()
	if _, ok := base.Deadline(); !ok {
		t.Fatal("the base context has no deadline")
	}
	ctx := WithoutDeadline(base)
	if d, ok := ctx.Deadline(); ok || !d.IsZero() {
		t.Errorf("Deadline() = %v, %v; want none", d, ok)
	}
	if ctx.Value(key{}) != "v" {
		t.Error("values do not flow through")
	}
	if ctx.Err() != nil {
		t.Error("Err before cancel")
	}
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancellation does not flow through")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("Err = %v", ctx.Err())
	}
	// a child made by the parser sees no deadline either
	child, cancel2 := context.WithCancel(WithoutDeadline(context.Background()))
	defer cancel2()
	if _, ok := child.Deadline(); ok {
		t.Error("a child context reports a deadline")
	}
}

func TestParserFacingReaderHasNoSeal(t *testing.T) {
	s := NewSealedReaderAt(bytes.NewReader([]byte("abcdef")), 0)
	r := s.Reader()
	if _, ok := r.(interface{ Seal() }); ok {
		t.Error("the reader handed to a parser exposes Seal")
	}
	if _, ok := r.(interface{ Close() error }); ok {
		t.Error("the reader handed to a parser exposes Close")
	}
	buf := make([]byte, 3)
	if n, err := r.ReadAt(buf, 1); n != 3 || err != nil || string(buf) != "bcd" {
		t.Errorf("ReadAt = %d, %v, %q", n, err, buf)
	}
	s.Seal()
	if _, err := r.ReadAt(buf, 0); !errors.Is(err, ErrSealed) {
		t.Errorf("a read through the view after the host's seal = %v, want ErrSealed", err)
	}
}

func TestParserFacingReaderMethodSetIsReadAtOnly(t *testing.T) {
	typ := reflect.TypeOf(NewSealedReaderAt(bytes.NewReader(nil), 0).Reader())
	if typ.NumMethod() != 1 || typ.Method(0).Name != "ReadAt" {
		t.Errorf("method set of the parser-facing reader has %d methods, first %q; want exactly ReadAt", typ.NumMethod(), typ.Method(0).Name)
	}
}
