package androidwhatsapp

import (
	"context"

	"github.com/rbenzing/minutiae/internal/parse"
)

// CappedParser is Parser with a test cap on the deleted_chat_job rows one parse reads (4I-R4 f).
type CappedParser struct {
	Parser
	Max int
}

// Parse is parseWith with the cap.
func (c CappedParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return parseWith(ctx, in, out, c.Max)
}
