package filesys_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

type baseFS struct{ filesys.FileSystem }

type recFS struct {
	baseFS
	tag string
}

func (r *recFS) Recoverable(filesys.Entry) ([]filesys.Candidate, error) { return nil, nil }

type wrapFS struct {
	baseFS
	inner filesys.FileSystem
}

func (w *wrapFS) Underlying() filesys.FileSystem { return w.inner }

func TestAsFindsDirectImplementation(t *testing.T) {
	r := &recFS{tag: "direct"}
	got, ok := filesys.As[filesys.Recoverer](r)
	if !ok || got != filesys.Recoverer(r) {
		t.Fatalf("As = %v, %v", got, ok)
	}
	if _, ok := filesys.As[filesys.Journaler](r); ok {
		t.Error("found a Journaler that is not there")
	}
	if _, ok := filesys.As[filesys.Recoverer](nil); ok {
		t.Error("a nil filesystem implements nothing")
	}
}

func TestAsWalksWrappers(t *testing.T) {
	r := &recFS{tag: "core"}
	chain := &wrapFS{inner: &wrapFS{inner: &wrapFS{inner: r}}}
	got, ok := filesys.As[filesys.Recoverer](chain)
	if !ok || got != filesys.Recoverer(r) {
		t.Fatalf("three wrappers: %v, %v", got, ok)
	}
	// A wrapper returning nil ends the walk.
	if _, ok := filesys.As[filesys.Recoverer](&wrapFS{}); ok {
		t.Error("nil Underlying found something")
	}
	// A cycle ends the walk.
	a := &wrapFS{}
	b := &wrapFS{inner: a}
	a.inner = b
	if _, ok := filesys.As[filesys.Recoverer](a); ok {
		t.Error("cycle found something")
	}
	self := &wrapFS{}
	self.inner = self
	if _, ok := filesys.As[filesys.Recoverer](self); ok {
		t.Error("self cycle found something")
	}
	// A modest chain is walked; one beyond the depth cap is not (the walk is bounded).
	deep := filesys.FileSystem(r)
	for range 10 {
		deep = &wrapFS{inner: deep}
	}
	if _, ok := filesys.As[filesys.Recoverer](deep); !ok {
		t.Error("a chain of 10 wrappers must be walked")
	}
	for range 200 {
		deep = &wrapFS{inner: deep}
	}
	if _, ok := filesys.As[filesys.Recoverer](deep); ok {
		t.Error("a chain of 210 wrappers must hit the depth cap")
	}
}

// The first implementation on the chain wins over what it wraps.
func TestAsFirstImplementationWins(t *testing.T) {
	inner := &recFS{tag: "inner"}
	outer := &outerRec{wrapFS: wrapFS{inner: inner}}
	got, ok := filesys.As[filesys.Recoverer](outer)
	if !ok || got != filesys.Recoverer(outer) {
		t.Fatalf("As = %v, %v", got, ok)
	}
}

type outerRec struct{ wrapFS }

func (*outerRec) Recoverable(filesys.Entry) ([]filesys.Candidate, error) { return nil, nil }

// A wrapper whose Underlying() is a typed nil pointer ends the chain: As must neither panic nor
// report a capability.
func TestAsTypedNilUnderlyingEndsTheChain(t *testing.T) {
	var nilWrap *wrapFS
	outer := &wrapFS{inner: nilWrap}
	if _, ok := filesys.As[filesys.Recoverer](outer); ok {
		t.Error("a typed-nil Underlying found a capability")
	}
	var nilRec *recFS
	got, ok := filesys.As[filesys.Recoverer](&wrapFS{inner: nilRec})
	if ok && got == nil {
		t.Error("As returned ok with a nil Recoverer")
	}
	// A typed-nil receiver at the head is also the end of the chain.
	if _, ok := filesys.As[filesys.Recoverer](nilWrap); ok {
		t.Error("a typed-nil filesystem found a capability")
	}
}

type askingFS struct {
	recFS
	answer bool
}

func (a *askingFS) SupportsRecovery() bool { return a.answer }

func TestSupportsRecovery(t *testing.T) {
	tests := []struct {
		name string
		fs   filesys.FileSystem
		want bool
	}{
		{"direct", &recFS{}, true},
		{"behind a wrapper", &wrapFS{inner: &recFS{}}, true},
		{"none", &baseFS{}, false},
		{"wrapper of none", &wrapFS{inner: &baseFS{}}, false},
		{"nil", nil, false},
		{"self-report yes", &askingFS{answer: true}, true},
		{"self-report no beats the Recoverable method", &askingFS{answer: false}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filesys.SupportsRecovery(tc.fs); got != tc.want {
				t.Errorf("SupportsRecovery = %v, want %v", got, tc.want)
			}
		})
	}
}
