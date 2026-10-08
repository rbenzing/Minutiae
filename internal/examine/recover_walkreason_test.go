package examine_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// C60: a walk error that is not corruption is an io-error skip, never labelled evidence corruption.
func TestRecoverWalkErrorReasonSeparatesCorruptFromIO(t *testing.T) {
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs)), {Path: "/dbad", Dir: true}, {Path: "/dio", Dir: true}}
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			switch dir.Name {
			case "dbad":
				return fmt.Errorf("injected: %w", filesys.ErrCorrupt)
			case "dio":
				return errors.New("injected: device read failed")
			}
			return nil
		}
	}, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["corrupt"] != 1 || sum.SkippedBy["io-error"] != 1 || sum.Recovered != 1 {
		t.Fatalf("summary %+v, want one corrupt, one io-error and the file recovered", sum)
	}
	if !anyContains(warnReasons(t, e.c), "io-error") {
		t.Errorf("warnings %q lack an io-error reason", warnReasons(t, e.c))
	}
}

// C60: beyond the cap the further walk errors keep their reasons in the counters.
func TestRecoverWalkErrorsBeyondCapKeepTheirReason(t *testing.T) {
	defer examine.SetWalkSkipCap(1)()
	nodes := []fstest.Node{delNode("/a.bin", pat(1, bs))}
	for i := range 4 {
		nodes = append(nodes, fstest.Node{Path: fmt.Sprintf("/d%d", i), Dir: true})
	}
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.failReadDir = func(dir filesys.Entry) error {
			if dir.Name == "d1" || dir.Name == "d3" {
				return fmt.Errorf("injected: %w", filesys.ErrCorrupt)
			}
			if strings.HasPrefix(dir.Name, "d") {
				return errors.New("injected: device read failed")
			}
			return nil
		}
	}, nodes...)
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.SkippedBy["corrupt"] != 2 || sum.SkippedBy["io-error"] != 2 {
		t.Errorf("skipped_by %v, want 2 corrupt and 2 io-error", sum.SkippedBy)
	}
}
