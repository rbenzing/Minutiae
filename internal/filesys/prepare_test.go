package filesys_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

type prepFS struct {
	baseFS
	calls int
	err   error
}

func (p *prepFS) PrepareRecovery(context.Context) error { p.calls++; return p.err }

func TestPrepareRecoveryFindsWrapped(t *testing.T) {
	core := &prepFS{}
	chain := &wrapFS{inner: &wrapFS{inner: core}}
	if err := filesys.PrepareRecovery(context.Background(), chain); err != nil {
		t.Fatal(err)
	}
	if core.calls != 1 {
		t.Fatalf("inner called %d times through two wrappers, want 1", core.calls)
	}
	// No preparer anywhere: nil, nothing to do.
	if err := filesys.PrepareRecovery(context.Background(), &wrapFS{inner: &recFS{}}); err != nil {
		t.Fatalf("no preparer: %v", err)
	}
	// A typed nil Underlying ends the chain.
	var typedNil *prepFS
	if err := filesys.PrepareRecovery(context.Background(), &wrapFS{inner: typedNil}); err != nil {
		t.Fatalf("typed nil: %v", err)
	}
	if err := filesys.PrepareRecovery(context.Background(), nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
}

func TestPrepareRecoveryReturnsInnerError(t *testing.T) {
	want := errors.New("boom")
	core := &prepFS{err: want}
	if err := filesys.PrepareRecovery(context.Background(), &wrapFS{inner: core}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the preparer's error", err)
	}
}
