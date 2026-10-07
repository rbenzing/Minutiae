package artparse

import (
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
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
