package records_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestNewValidatorJudgesLikeAddAndWritesNothing (J117): the validator accepts
// what Add accepts, refuses what Add refuses with the same typed errors, and
// leaves the case untouched (no audit entry).
func TestNewValidatorJudgesLikeAddAndWritesNothing(t *testing.T) {
	c, a := setup(t)
	before, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	validate, err := records.NewValidator(c, []string{a.ID})
	if err != nil {
		t.Fatal(err)
	}
	good := recordstest.Records(a.ID, 1, 1)[0]
	good.Deleted, good.Recovery, good.Range = false, "", nil
	if err := validate(good); err != nil {
		t.Errorf("a valid record: %v", err)
	}
	bad := good
	bad.ArtifactID = "no-such-artifact"
	if err := validate(bad); !errors.Is(err, records.ErrUnknownArtifact) || !errors.Is(err, records.ErrInvalidRecord) {
		t.Errorf("unknown artifact: %v", err)
	}
	bad = good
	bad.Type = "no-such-type"
	if err := validate(bad); !errors.Is(err, records.ErrUnknownType) {
		t.Errorf("unknown type: %v", err)
	}
	bad = good
	bad.Range = &records.Range{Offset: 0, Length: 1 << 40}
	if err := validate(bad); !errors.Is(err, records.ErrInvalidRange) {
		t.Errorf("range past the artifact: %v", err)
	}
	if _, err := records.NewValidator(c, []string{"no-such-artifact"}); !errors.Is(err, records.ErrUnknownArtifact) {
		t.Errorf("undeclared artifact set: %v", err)
	}
	after, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the validator appended %d audit entries", len(after)-len(before))
	}
}
