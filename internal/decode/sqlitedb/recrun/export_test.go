package recrun

import (
	"context"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// RunWith is Run with the facts function and the cap on invalid-facts warnings replaced.
func RunWith(ctx context.Context, in *parse.Input, out parse.RecoveryEmitter, mappings []parse.TableMapping, claims []parse.TableClaim, f func(sqlitefile.RecoveredRow) parse.RecoveredFacts, maxWarn int) (Report, error) {
	return run(ctx, in, out, mappings, claims, f, maxWarn)
}
