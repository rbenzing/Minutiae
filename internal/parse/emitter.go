package parse

import (
	"context"

	"github.com/rbenzing/minutiae/internal/records"
)

// Emitter is how records leave a parser. The host implements it; a parser
// never holds a writer.
type Emitter interface {
	// Emit hands over one record. A rejected record is warned, counted and
	// the call returns nil; fatal errors (caps, a cancelled job, a failing
	// case) are returned and must stop the parser.
	Emit(ctx context.Context, r records.Record) error
	// Warn reports something the parser could not extract. locator and
	// reason are bounded and cleaned by the host.
	Warn(ctx context.Context, locator, reason string) error
	// Note records a bounded key/value fact about the job.
	Note(key, value string)
	// Progress reports done of total units of work.
	Progress(done, total int64)
}
