package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewArtifactRefusesRecoveredKindWithoutRecovery(t *testing.T) {
	for _, kind := range []string{KindRecover, KindCarve, KindSlack, KindJournal, KindReport} {
		for name, d := range map[string]*Derivation{"nil derived": nil, "nil recovery": {ParentID: "x"}} {
			c := newTestCase(t)
			before, _ := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
			_, err := c.NewArtifact("dev1", "acq1", "recovered/x", Source{Kind: kind, DeviceID: "dev1", Derived: d})
			if !errors.Is(err, ErrRecoveredKindNeedsRecovery) {
				t.Errorf("%s/%s: err = %v, want ErrRecoveredKindNeedsRecovery", kind, name, err)
			}
			after, _ := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
			if len(after) != len(before) {
				t.Errorf("%s/%s: audit grew on a refused artifact", kind, name)
			}
			if _, serr := os.Stat(filepath.Join(c.Dir, "artifacts", "dev1", "acq1", "recovered", "x")); serr == nil {
				t.Errorf("%s/%s: a file was created", kind, name)
			}
		}
	}
}
