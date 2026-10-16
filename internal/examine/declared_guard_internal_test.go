package examine

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// X29: the full run list is linked unless it is the captured sidecar itself,
// and the error names the full list either way.
func TestNoteFullRunsGuard(t *testing.T) {
	base := errors.New("chunk corrupt")
	t.Run("a full list distinct from the captured sidecar is linked and named", func(t *testing.T) {
		d := &evidence.Derivation{RunsArtifact: "prefix-sidecar"}
		err := noteFullRuns(d, "full-sidecar", base)
		if d.DeclaredRunsArtifact != "full-sidecar" || !strings.Contains(err.Error(), "full-sidecar") || !errors.Is(err, base) {
			t.Fatalf("link %q, err %v", d.DeclaredRunsArtifact, err)
		}
	})
	t.Run("a full list equal to the captured sidecar is not linked but still named", func(t *testing.T) {
		d := &evidence.Derivation{RunsArtifact: "same"}
		err := noteFullRuns(d, "same", base)
		if d.DeclaredRunsArtifact != "" || !strings.Contains(err.Error(), "runs artifact same") {
			t.Fatalf("link %q, err %v", d.DeclaredRunsArtifact, err)
		}
	})
	t.Run("no full list changes nothing", func(t *testing.T) {
		d := &evidence.Derivation{}
		if err := noteFullRuns(d, "", base); err != base || d.DeclaredRunsArtifact != "" {
			t.Fatalf("link %q, err %v", d.DeclaredRunsArtifact, err)
		}
	})
}
