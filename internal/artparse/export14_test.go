package artparse

import (
	"context"

	"github.com/rbenzing/minutiae/internal/parse"
)

func WithAfterSize(o *Options, f func(string)) { o.afterSize = f }

// Bundle exposes the unexported bundle to the external tests.
type Bundle = bundle

func OpenBundle(ctx context.Context, h *Host, snap *Snapshot, j Job, parseID string) (*Bundle, error) {
	b, err := h.openBundle(ctx, snap, j, parseID)
	if err != nil {
		return nil, err // never a typed nil
	}
	return b, nil
}
func (b *bundle) Input(forProbe bool) *parse.Input     { return b.input(forProbe) }
func (b *bundle) Seal()                                { b.seal() }
func (b *bundle) Recheck(ctx context.Context) error    { return b.recheck(ctx) }
func (b *bundle) RecheckManifest(snap *Snapshot) error { return b.recheckManifest(snap) }
func (b *bundle) Lookups() []LookupOpen                { return b.lookups() }
func (b *bundle) Close()                               { b.close() }
func (h *Host) Limits() parse.Limits                   { return h.limits }
