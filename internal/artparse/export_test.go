package artparse

import (
	"context"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
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
	if len(c) > 0 && r.recoverHash == "" { // New wants a recovery identity for any claim
		r.recoverHash = "src1:sha256:" + strings.Repeat("c", 64)
	}
	return r
}

func WithBatchRows(o *Options, n int) { o.batchRows = n }

// WithAuditHook installs a function called before every audit entry the host appends; an error refuses
// the append.
func WithAuditHook(o *Options, f func(action string, d map[string]any) error) { o.auditHook = f }

// SetNewWriterForTest replaces the writer factory of a built host.
func SetNewWriterForTest(h *Host, f func(*evidence.Case, records.Parser, records.WriterOptions) (IngestWriter, error)) {
	h.newWriter = f
}

// SetRecheckTimeout shortens the budget of the post-job re-hash; call the result to restore it.
func SetRecheckTimeout(d time.Duration) (restore func()) {
	old := recheckTimeout
	recheckTimeout = d
	return func() { recheckTimeout = old }
}

// SetConcludeJoinTimeout shortens the wait Run applies to a pending conclusion; call the result to restore it.
func SetConcludeJoinTimeout(d time.Duration) (restore func()) {
	old := concludeJoinTimeout
	concludeJoinTimeout = d
	return func() { concludeJoinTimeout = old }
}

// SetJobContext replaces the derivation of the time-limited context of every guarded call (Probe and
// Parse); call the result to restore it. Tests use it to expire a job on an event, not after a delay.
func SetJobContext(f func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)) (restore func()) {
	old := newJobCtx
	newJobCtx = f
	return func() { newJobCtx = old }
}

// Backup resolution seams.
func SetMaxFilesRows(o *Options, n int)                               { o.maxFilesRows = n }
func WithResolveBudget(o *Options, f func(limit int64) *parse.Budget) { o.resolveBudget = f }
func WithAfterManifestRead(o *Options, f func())                      { o.afterManifestRead = f }
func NameCalls(h *Host) int64                                         { return h.nameCalls.Load() }
func FindScan(b *Bundle, glob string) []parse.Artifact                { return (&lookuper{b: b}).findScan(glob) }

// DisablePropagation makes the emitters of a built host skip the propagation of recovered-ness, so a
// test can see the writer's own rule refuse a live record on a recovered artifact.
func DisablePropagation(h *Host) { h.noPropagate = true }

// WithMaxUnmapped lowers the unmapped-locator bound of every recovery job of the host.
func WithMaxUnmapped(o *Options, n int) { o.maxUnmapped = n }

// WithTypedRunner replaces the typed recovery runner (recrun.Run) of the host.
func WithTypedRunner(o *Options, f func(context.Context, *parse.Input, parse.RecoveryEmitter, []parse.TableMapping, []parse.TableClaim) (recrun.Report, error)) {
	o.typedRun = f
}

// WithScanMaxSize lowers the size above which --scan counts an artifact instead of probing it.
func WithScanMaxSize(o *Options, n int64) { o.scanMaxSize = n }

// WithMaxScanFindings lowers the number of scan findings a plan keeps.
func WithMaxScanFindings(o *Options, n int) { o.maxScanFindings = n }

// AllParserReason is the classifier of pre-probe reasons that hold for every parser.
var AllParserReason = allParserReason
