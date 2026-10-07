package records_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestCorruptIndexIsAnIntegrityError (R64): a damaged full-text structure surfaces from every
// Reader call that reads the index as evidence.ErrIntegrity with a message that names `case verify`,
// never as a bare driver error.
func TestCorruptIndexIsAnIntegrityError(t *testing.T) {
	c, art := setup(t)
	recs := recordstest.Records(art.ID, 30, 3)
	for i := range recs {
		recs[i].Summary = fmt.Sprintf("common token %d", i)
		recs[i].Body = "common words in the body of the record"
	}
	recordstest.Ingest(t, c, testParser, []string{art.ID}, recs)
	r := newReader(t, c)
	recordstest.WreckFTSStructure(t, c.Dir, evidence.FTSWordTable)
	for name, call := range textCalls(t, r) {
		t.Run(name, func(t *testing.T) {
			err := call(ctx)
			if !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("%v, want evidence.ErrIntegrity", err)
			}
			if !strings.Contains(err.Error(), "case verify") {
				t.Errorf("%v does not name case verify", err)
			}
			if errors.Is(err, records.ErrSearchTimeout) {
				t.Errorf("%v is a timeout", err)
			}
		})
	}
	// the case itself is intact: a call without text still works
	if _, err := r.List(ctx, records.Filter{}, records.Page{Limit: 5}); err != nil {
		t.Errorf("List without text: %v", err)
	}
}
