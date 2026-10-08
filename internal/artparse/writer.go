package artparse

import (
	"context"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// ingestWriter is the small interface artparse needs from the records writer;
// *records.Writer satisfies it.
type ingestWriter interface {
	Start(ctx context.Context, so records.StartOptions) error
	Add(ctx context.Context, r records.Record) error
	Warn(ctx context.Context, path, reason string) error
	Reject(ctx context.Context, path, reason string) error
	Flush(ctx context.Context) error
	End(ctx context.Context) (records.IngestResult, error)
	Abort(ctx context.Context, cause error) (records.IngestResult, error)
	IngestID() string
}

var _ ingestWriter = (*records.Writer)(nil)

// writerFactory makes the writer of one job.
type writerFactory func(c *evidence.Case, p records.Parser, o records.WriterOptions) (ingestWriter, error)

func newRecordsWriter(c *evidence.Case, p records.Parser, o records.WriterOptions) (ingestWriter, error) {
	return records.NewWriter(c, p, o)
}
