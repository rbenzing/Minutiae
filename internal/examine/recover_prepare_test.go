package examine_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// prepEnv is a hook environment whose events (readdir, prepare, recoverable) are logged in order.
func prepEnv(t *testing.T, prepare func(ctx context.Context) error, nodes ...fstest.Node) (*recEnv, *[]string) {
	t.Helper()
	var log []string
	e, _ := newHookEnv(t, func(h *fsHook) {
		h.onReadDir = func(filesys.Entry, []filesys.Entry) { log = append(log, "readdir") }
		h.prepare = func(ctx context.Context) error {
			log = append(log, "prepare")
			if prepare != nil {
				return prepare(ctx)
			}
			return nil
		}
		h.recoverable = func(inner filesys.Recoverer, en filesys.Entry) ([]filesys.Candidate, error) {
			log = append(log, "recoverable")
			return inner.Recoverable(en)
		}
	}, nodes...)
	return e, &log
}

func TestPrepareRecoveryCalledOnceAfterEnumerate(t *testing.T) {
	e, log := prepEnv(t, nil, delNode("/a.bin", pat(1, bs)), delNode("/b.bin", pat(2, bs)),
		fstest.Node{Path: "/dir", Dir: true}, delNode("/dir/c.bin", pat(3, bs)))
	e.recover(t, examine.RecoverOptions{All: true})
	i := slices.Index(*log, "prepare")
	if i < 0 || slices.Contains((*log)[i+1:], "prepare") {
		t.Fatalf("prepare must run exactly once: %v", *log)
	}
	if slices.Contains((*log)[i:], "readdir") {
		t.Errorf("a directory was read after prepare (enumeration must be over): %v", *log)
	}
	if !slices.Contains((*log)[:i], "readdir") || slices.Contains((*log)[:i], "recoverable") {
		t.Errorf("prepare must follow the walk and precede the first Recoverable: %v", *log)
	}
	if !slices.Contains((*log)[i:], "recoverable") {
		t.Errorf("no Recoverable after prepare: %v", *log)
	}
}

func TestPrepareRecoveryNotCalledForListOrNoTargets(t *testing.T) {
	e, log := prepEnv(t, nil, delNode("/a.bin", pat(1, bs)))
	if _, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*log, "prepare") {
		t.Errorf("PlanRecovery (--list) called prepare: %v", *log)
	}
	e2, log2 := prepEnv(t, nil, fstest.Node{Path: "/live.bin", Data: pat(2, bs)})
	e2.recover(t, examine.RecoverOptions{All: true})
	if slices.Contains(*log2, "prepare") {
		t.Errorf("Recover with no deleted entry called prepare: %v", *log2)
	}
}

func TestPrepareRecoveryCancelEndsRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, _ := prepEnv(t, func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}, delNode("/a.bin", pat(1, bs)))
	_, err := e.s.Recover(ctx, examine.RecoverOptions{Partition: -1, All: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recover = %v, want context.Canceled", err)
	}
	if n := len(recovered(t, e.c)); n != 0 {
		t.Errorf("%d artifacts after a cancelled preparation, want none", n)
	}
	es := auditEntries(t, e.c)
	if got := es[len(es)-1].Action; got != "analysis.error" {
		t.Errorf("last audit action = %q, want analysis.error", got)
	}
}

func TestPrepareRecoveryPanicIsCorrupt(t *testing.T) {
	e, _ := prepEnv(t, func(context.Context) error { panic("hostile free-space pass") }, delNode("/a.bin", pat(1, bs)))
	_, err := e.s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("Recover = %v, want filesys.ErrCorrupt", err)
	}
	if n := len(recovered(t, e.c)); n != 0 {
		t.Errorf("%d artifacts after a panicking preparation, want none", n)
	}
}
