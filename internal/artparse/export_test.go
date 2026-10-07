package artparse

import (
	"context"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// Test seams: the only way the unexported options are reachable.
type (
	CaseSource   = caseSource
	IngestWriter = ingestWriter
)

func SkipLimitMinimums(o *Options)            { o.skipLimitMinimums = true }
func WithCaseSource(o *Options, s CaseSource) { o.source = s }
func WithNewWriter(o *Options, f func(*evidence.Case, records.Parser, records.WriterOptions) (IngestWriter, error)) {
	o.newWriter = f
}
func WithOnOpen(o *Options, f func(string)) { o.onOpen = f }
func WithAfterStart(o *Options, f func())   { o.afterStart = f }

// SetMetaTimeout shortens the Meta() bound of Register; call the result to restore it.
func SetMetaTimeout(d time.Duration) (restore func()) {
	old := metaTimeout
	metaTimeout = d
	return func() { metaTimeout = old }
}

var AcquisitionKey = acquisitionKey

// SetRegistry replaces the registry of a built host without the checks New makes, so a test can
// plan with a parser that would not pass them.
func SetRegistry(h *Host, ps ...Registered) { h.registry = ps }

// WithEmits returns r with its declared emits replaced.
func WithEmits(r Registered, e []parse.Emit) Registered {
	r.meta.Emits = e
	return r
}

// Prepared is the outcome of the shared front half of a job.
type Prepared struct {
	Bundle    *Bundle
	Status    string
	Reason    string
	Integrity bool
}

// PrepareJob runs the front half of a job (the one Plan and Run share).
func PrepareJob(ctx context.Context, h *Host, snap *Snapshot, j Job, parseID string) (Prepared, error) {
	pr, err := h.prepareJob(ctx, snap, j, parseID)
	return Prepared{Bundle: pr.bundle, Status: pr.status, Reason: pr.reason, Integrity: pr.integrity}, err
}

// Case returns the host's case.
func (h *Host) Case() *evidence.Case { return h.c }

// WithClaims returns r with its declared table claims replaced (no validation).
func WithClaims(r Registered, c []parse.TableClaim) Registered {
	r.meta.Claims = c
	return r
}
