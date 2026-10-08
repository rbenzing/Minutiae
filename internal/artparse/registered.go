// Package artparse is the host of the artifact parsers: it registers parsers,
// names manifest artifacts, discovers jobs and (later tasks) runs them.
package artparse

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// metaTimeout bounds the one Meta() call Register makes.
var metaTimeout = 5 * time.Second

var hashRE = regexp.MustCompile(`^src1:sha256:[0-9a-f]{64}$`)

// Registered is a parser with its source hash and a deep copy of its Meta,
// read once at Register.
type Registered struct {
	p    parse.Parser
	hash string
	meta parse.Meta
}

// Register reads p.Meta() once, on its own goroutine under recover with a
// timeout (a parser that panics or hangs in Meta() is a Register error), deep
// copies it, validates it and requires hash to be a parser source hash.
func Register(p parse.Parser, hash string) (Registered, error) {
	if p == nil {
		return Registered{}, errors.New("artparse: nil parser")
	}
	if !hashRE.MatchString(hash) {
		return Registered{}, fmt.Errorf("artparse: parser hash %q is not src1:sha256:<64 hex digits>", hash)
	}
	type result struct {
		m   parse.Meta
		err error
	}
	ch := make(chan result, 1) // buffered: a late answer never blocks the goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("artparse: Meta() panicked: %v", r)}
			}
		}()
		ch <- result{m: p.Meta().Clone()}
	}()
	timer := time.NewTimer(metaTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			return Registered{}, res.err
		}
		if err := parse.ValidateMeta(res.m); err != nil {
			return Registered{}, err
		}
		return Registered{p: p, hash: hash, meta: res.m}, nil
	case <-timer.C:
		return Registered{}, fmt.Errorf("artparse: Meta() did not return within %v", metaTimeout)
	}
}

// Meta returns a fresh copy of the registered Meta.
func (r Registered) Meta() parse.Meta { return r.meta.Clone() }

// Identity is the parser identity the records writer stores.
func (r Registered) Identity() records.Parser {
	return records.Parser{Name: r.meta.Name, Version: r.meta.Version, Hash: r.hash}
}

// Parser returns the parser itself.
func (r Registered) Parser() parse.Parser { return r.p }

// Check refuses a parser that emits a record type without a payload contract
// (ErrNoPayloadContract) or at another payload version than the registered one
// (ErrPayloadVersionMismatch).
func (r Registered) Check() error {
	for _, e := range r.meta.Emits {
		t, ok := records.LookupType(e.Type)
		if !ok || t.Validate == nil {
			return fmt.Errorf("parser %s: emits %q: %w", r.meta.Name, e.Type, parse.ErrNoPayloadContract)
		}
		if t.PayloadVersion != e.PayloadVersion {
			return fmt.Errorf("parser %s: emits %q at payload version %d, registered version is %d: %w",
				r.meta.Name, e.Type, e.PayloadVersion, t.PayloadVersion, parse.ErrPayloadVersionMismatch)
		}
	}
	return nil
}
