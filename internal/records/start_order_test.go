package records_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// A manifest id held by two records is an integrity error, as for OpenArtifact: Start never picks
// one of them (the recovery trail would depend on which), and audits nothing.
func TestStartRefusesDuplicateManifestID(t *testing.T) {
	c, a := setup(t)
	recordstest.DuplicateManifestRecord(t, c.Dir, a.ID)
	before := len(auditOf(t, c, ""))
	w := newWriter(t, c, testParser, records.WriterOptions{})
	err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
	if !errors.Is(err, evidence.ErrIntegrity) || !strings.Contains(err.Error(), "appears more than once in the manifest") {
		t.Fatalf("Start with a duplicate manifest id = %v, want ErrIntegrity naming the duplicate", err)
	}
	if after := len(auditOf(t, c, "")); after != before {
		t.Fatalf("the refused Start audited %d entries", after-before)
	}
}

// C14 order: the schema objects and the index are checked before the manifest (and so the recovery
// trail) is read. With a duplicated manifest id AND a tampered schema (or index), the refusal is the
// schema's (the index's), never the manifest's, and nothing is audited.
func TestStartChecksSchemaAndIndexBeforeTheManifest(t *testing.T) {
	for name, tc := range map[string]struct {
		tamper func(t *testing.T, c *evidence.Case)
		want   func(error) bool
	}{
		"schema": {
			func(t *testing.T, c *evidence.Case) { recordstest.DropTrigger(t, c.Dir, "immut_records_upd") },
			func(err error) bool { return errors.Is(err, evidence.ErrIntegrity) },
		},
		"index": {
			func(t *testing.T, c *evidence.Case) { recordstest.SetFTSNormVersion(t, c.Dir, "building") },
			func(err error) bool { return errors.Is(err, records.ErrIndexNotCurrent) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, a := setup(t)
			recordstest.DuplicateManifestRecord(t, c.Dir, a.ID)
			tc.tamper(t, c)
			before := len(auditOf(t, c, ""))
			w := newWriter(t, c, testParser, records.WriterOptions{})
			err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
			if err == nil || !tc.want(err) || strings.Contains(err.Error(), "appears") {
				t.Fatalf("Start = %v; want the %s refusal, not the manifest's", err, name)
			}
			if after := len(auditOf(t, c, "")); after != before {
				t.Fatalf("the refused Start audited %d entries", after-before)
			}
		})
	}
}
