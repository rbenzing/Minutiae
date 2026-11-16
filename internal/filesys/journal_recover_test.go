package filesys_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

func TestParseJournalUse(t *testing.T) {
	for in, want := range map[string]filesys.JournalUse{
		"": filesys.JournalUseNone, "none": filesys.JournalUseNone,
		"committed": filesys.JournalUseCommitted, "stale": filesys.JournalUseStale,
	} {
		got, err := filesys.ParseJournalUse(in)
		if err != nil || got != want {
			t.Errorf("ParseJournalUse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Stale", "all", "None", " stale", "committed "} {
		if got, err := filesys.ParseJournalUse(in); err == nil {
			t.Errorf("ParseJournalUse(%q) = %q, nil; want an error", in, got)
		}
	}
}

type jrecFS struct {
	baseFS
	calls int
	use   filesys.JournalUse
	err   error
}

func (p *jrecFS) PrepareJournalRecovery(_ context.Context, use filesys.JournalUse) error {
	p.calls++
	p.use = use
	return p.err
}

func TestPrepareJournalRecoveryFindsWrapped(t *testing.T) {
	ctx := context.Background()
	core := &jrecFS{}
	chain := &wrapFS{inner: &wrapFS{inner: core}}
	if err := filesys.PrepareJournalRecovery(ctx, chain, filesys.JournalUseStale); err != nil {
		t.Fatal(err)
	}
	if core.calls != 1 || core.use != filesys.JournalUseStale {
		t.Fatalf("inner called %d times with %q through two wrappers, want once with stale", core.calls, core.use)
	}
	// none (and "") is forwarded as none, so a reader drops its preparation; a filesystem without the implementation says nil
	for _, use := range []filesys.JournalUse{filesys.JournalUseNone, ""} {
		if err := filesys.PrepareJournalRecovery(ctx, chain, use); err != nil {
			t.Fatalf("use %q: %v", use, err)
		}
		if err := filesys.PrepareJournalRecovery(ctx, &wrapFS{inner: &recFS{}}, use); err != nil {
			t.Fatalf("use %q, no implementation: %v", use, err)
		}
	}
	if core.calls != 3 || core.use != filesys.JournalUseNone {
		t.Fatalf("none was forwarded %d times (last use %q); want twice, as none", core.calls-1, core.use)
	}
	// absent implementation
	err := filesys.PrepareJournalRecovery(ctx, &wrapFS{inner: &recFS{}}, filesys.JournalUseStale)
	if !errors.Is(err, filesys.ErrNoJournalRecovery) || !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("absent: %v, want ErrNoJournalRecovery (wrapping ErrUnsupported)", err)
	}
	if err := filesys.PrepareJournalRecovery(ctx, nil, filesys.JournalUseCommitted); !errors.Is(err, filesys.ErrNoJournalRecovery) {
		t.Fatalf("nil filesystem: %v", err)
	}
	// the inner error comes back
	want := errors.New("boom")
	if err := filesys.PrepareJournalRecovery(ctx, &jrecFS{err: want}, filesys.JournalUseCommitted); !errors.Is(err, want) {
		t.Fatalf("inner error: %v", err)
	}
}

type supportsJrnFS struct {
	baseFS
	answer bool
}

func (s *supportsJrnFS) SupportsJournalRecovery() bool { return s.answer }

func TestSupportsJournalRecoveryHonoursWrapperAnswer(t *testing.T) {
	if !filesys.SupportsJournalRecovery(&wrapFS{inner: &jrecFS{}}) {
		t.Error("an implementation under a wrapper is not found")
	}
	if filesys.SupportsJournalRecovery(&wrapFS{inner: &recFS{}}) {
		t.Error("no implementation reported as supported")
	}
	if filesys.SupportsJournalRecovery(nil) {
		t.Error("nil reported as supported")
	}
	// the wrapper's own answer wins over what it wraps, both ways
	if filesys.SupportsJournalRecovery(&supportsJrnFS{answer: false}) {
		t.Error("a wrapper saying false was overridden")
	}
	if !filesys.SupportsJournalRecovery(&supportsJrnFS{answer: true}) {
		t.Error("a wrapper saying true was overridden")
	}
}

// denyingFS implements JournalRecoverer but says, as a wrapper may, that it cannot.
type denyingFS struct{ jrecFS }

func (denyingFS) SupportsJournalRecovery() bool { return false }

func TestSupportsJournalRecoveryWrapperFalseBeatsImplementation(t *testing.T) {
	if filesys.SupportsJournalRecovery(&denyingFS{}) {
		t.Error("a wrapper saying false was overridden by the method it also has")
	}
}

func TestCheckCandidateJournalRegion(t *testing.T) {
	const fsSize = 10000
	for _, region := range []string{"live", "stale"} {
		c := cand(100, filesys.Run{Offset: 512, Length: 100})
		c.Journal = &filesys.CandidateJournal{Seq: 7, Committed: true, Region: region}
		if got, err := filesys.CheckCandidate(c, fsSize); err != nil || got != 100 {
			t.Errorf("region %q: covered %d, %v; want 100, nil", region, got, err)
		}
	}
	for _, region := range []string{"", "other", "Live"} {
		c := cand(100, filesys.Run{Offset: 512, Length: 100})
		c.Journal = &filesys.CandidateJournal{Seq: 7, Committed: true, Region: region}
		_, err := filesys.CheckCandidate(c, fsSize)
		var ce *filesys.CorruptError
		if !errors.As(err, &ce) || ce.Structure != "recovery map" {
			t.Errorf("region %q: %v, want a recovery map CorruptError", region, err)
		}
	}
	// no Journal: unchanged
	if _, err := filesys.CheckCandidate(cand(100, filesys.Run{Offset: 512, Length: 100}), fsSize); err != nil {
		t.Errorf("nil Journal: %v", err)
	}
}
