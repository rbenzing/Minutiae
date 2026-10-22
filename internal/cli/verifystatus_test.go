package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

var statusNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// stateMoved is a statusState that last saw seq and whose counters last moved ago before statusNow.
func stateMoved(seq uint64, ago time.Duration) *statusState {
	return &statusState{seen: true, lastSeq: seq, lastMove: statusNow.Add(-ago)}
}

func countedSnap(phase string, done, total int64, unit string, running time.Duration) evidence.VerifyProgressSnapshot {
	return evidence.VerifyProgressSnapshot{Phase: phase, Started: statusNow.Add(-running), Counted: true, Done: done, Total: total, Unit: unit, Seq: 5}
}

func TestRenderStatusCountedPhase(t *testing.T) {
	s := countedSnap("records.stream", 4000000, 10000000, "records", 100*time.Second)
	line := renderStatus(s, stateMoved(5, time.Second), statusNow, false, true)
	for _, want := range []string{"records.stream", "4,000,000 / 10,000,000 records", "40,000/s", "ETA 2m30s", "(last progress 1s ago)"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
}

func TestRenderStatusUncountedPhase(t *testing.T) {
	s := evidence.VerifyProgressSnapshot{Phase: "records.integrity_check", Started: statusNow.Add(-12 * time.Second), Seq: 3}
	line := renderStatus(s, stateMoved(3, 12*time.Second), statusNow, false, true)
	if !strings.Contains(line, "elapsed 12s") || !strings.Contains(line, "no inner progress for this phase") || strings.Contains(line, "ETA") {
		t.Errorf("line %q", line)
	}
}

func TestRenderStatusStall(t *testing.T) {
	s := countedSnap("records.stream", 100, 1000, "records", 300*time.Second)
	line := renderStatus(s, stateMoved(5, 130*time.Second), statusNow, false, true)
	if !strings.Contains(line, "no progress for 2m10s") {
		t.Errorf("line %q lacks the stall text", line)
	}
	for _, bad := range []string{"ETA", "last progress", "hung", "stuck", "healthy"} {
		if strings.Contains(line, bad) {
			t.Errorf("line %q contains %q", line, bad)
		}
	}
	// just below the threshold nothing is claimed
	line = renderStatus(s, stateMoved(5, 59*time.Second), statusNow, false, true)
	if strings.Contains(line, "no progress for") || !strings.Contains(line, "last progress 59s ago") {
		t.Errorf("line below the threshold: %q", line)
	}
}

func TestRenderStatusUncountedStallUsesLongerThreshold(t *testing.T) {
	mk := func(running time.Duration) string {
		s := evidence.VerifyProgressSnapshot{Phase: "records.integrity_check", Started: statusNow.Add(-running), Seq: 3}
		return renderStatus(s, stateMoved(3, running), statusNow, false, true)
	}
	if line := mk(9*time.Minute + 59*time.Second); strings.Contains(line, "no progress for") {
		t.Errorf("9m59s: %q", line)
	}
	if line := mk(11 * time.Minute); !strings.Contains(line, "no progress for 11m0s") {
		t.Errorf("11m0s: %q", line)
	}
}

func TestRenderStatusTotalUnknown(t *testing.T) {
	s := countedSnap("records.stream", 1234, -1, "records", 10*time.Second)
	line := renderStatus(s, stateMoved(5, time.Second), statusNow, false, true)
	if !strings.Contains(line, "1,234 records (total unknown)") || strings.Contains(line, "ETA") || strings.Contains(line, "/ -1") {
		t.Errorf("line %q", line)
	}
}

func TestRenderStatusDoneBeyondTotal(t *testing.T) {
	s := countedSnap("records.stream", 4100, 4000, "records", 10*time.Second)
	line := renderStatus(s, stateMoved(5, time.Second), statusNow, false, true)
	if !strings.Contains(line, "4,100 records (more than the 4,000 audited)") || strings.Contains(line, "ETA") || strings.Contains(line, "%") {
		t.Errorf("line %q", line)
	}
}

func TestRenderStatusBetweenPhases(t *testing.T) {
	line := renderStatus(evidence.VerifyProgressSnapshot{Seq: 9}, stateMoved(9, 4*time.Second), statusNow, false, true)
	if !strings.Contains(line, "other checks  elapsed 4s") {
		t.Errorf("line %q", line)
	}
}

func TestRenderStatusCancelling(t *testing.T) {
	s := countedSnap("records.stream", 100, 1000, "records", 10*time.Second)
	if line := renderStatus(s, stateMoved(5, time.Second), statusNow, true, true); !strings.Contains(line, "cancelling…") {
		t.Errorf("unicode: %q", line)
	}
	line := renderStatus(s, stateMoved(5, time.Second), statusNow, true, false)
	if !strings.Contains(line, "cancelling...") || strings.Contains(line, "…") {
		t.Errorf("ascii: %q", line)
	}
}

func TestRenderStatusFreshPhaseHasNoInventedETA(t *testing.T) {
	// a phase that has just started has no rate yet: no division by zero, no invented ETA
	s := countedSnap("artifacts", 0, 100, "bytes", 0)
	line := renderStatus(s, stateMoved(5, 0), statusNow, false, true)
	for _, bad := range []string{"ETA", "Inf", "NaN"} {
		if strings.Contains(line, bad) {
			t.Errorf("line %q contains %q", line, bad)
		}
	}
}

type fakeSource struct {
	mu sync.Mutex
	s  evidence.VerifyProgressSnapshot
}

func (f *fakeSource) Snapshot() evidence.VerifyProgressSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s
}

func (f *fakeSource) bumpSeq() {
	f.mu.Lock()
	f.s.Seq++
	f.mu.Unlock()
}

// glyphsDrawn is the set of first runes of the frames that name the phase.
func glyphsDrawn(out string) map[rune]bool {
	g := map[rune]bool{}
	for _, f := range strings.Split(out, "\r") {
		if strings.Contains(f, "records.stream") {
			g[[]rune(f)[0]] = true
		}
	}
	return g
}

func TestStatusSpinnerMovesOnlyWithProgress(t *testing.T) {
	now := func() time.Time { return statusNow }
	src := &fakeSource{s: countedSnap("records.stream", 1, 10, "records", time.Second)}
	var buf bytes.Buffer
	tick := make(chan time.Time)
	stop := startStatus(&buf, src, tick, now, nil, true, 0)
	for range 4 { // the fourth send returns only once the third frame is drawn
		tick <- statusNow
	}
	stop()
	if g := glyphsDrawn(buf.String()); len(g) != 1 {
		t.Fatalf("an unchanged Seq drew %d glyphs: %q", len(g), buf.String())
	}

	buf.Reset()
	tick = make(chan time.Time)
	stop = startStatus(&buf, src, tick, now, nil, true, 0)
	tick <- statusNow
	tick <- statusNow // the first frame is drawn
	src.bumpSeq()
	tick <- statusNow
	tick <- statusNow
	stop()
	if g := glyphsDrawn(buf.String()); len(g) != 2 {
		t.Fatalf("a Seq change drew %d glyphs, want 2: %q", len(g), buf.String())
	}
}

func TestStatusStopClearsTheLineAndNothingFollows(t *testing.T) {
	now := func() time.Time { return statusNow }
	src := &fakeSource{s: countedSnap("records.stream", 1, 10, "records", time.Second)}
	var buf bytes.Buffer
	tick := make(chan time.Time)
	stop := startStatus(&buf, src, tick, now, nil, true, 0)
	tick <- statusNow
	tick <- statusNow
	stop()
	out := buf.String()
	if !strings.HasSuffix(out, "\r") {
		t.Fatalf("output %q does not end in the clear sequence", out)
	}
	if last := out[strings.LastIndex(out[:len(out)-1], "\r"):]; strings.TrimSpace(last) != "" {
		t.Errorf("the clear sequence is %q, want only spaces between two returns", last)
	}
	// the first frame is drawn at start, before any tick, and stop clears it
	var first bytes.Buffer
	startStatus(&first, src, make(chan time.Time), now, nil, true, 0)()
	if !strings.Contains(first.String(), "records.stream") || !strings.HasSuffix(first.String(), "") {
		t.Errorf("no tick: %q, want a pre-drawn frame and the clear sequence", first.String())
	}
}

func TestStatusCancelledChannelShowsCancelling(t *testing.T) {
	src := &fakeSource{s: countedSnap("records.stream", 1, 10, "records", time.Second)}
	var buf bytes.Buffer
	tick := make(chan time.Time)
	cancelled := make(chan struct{})
	close(cancelled)
	stop := startStatus(&buf, src, tick, func() time.Time { return statusNow }, cancelled, false, 0)
	tick <- statusNow
	tick <- statusNow
	stop()
	if !strings.Contains(buf.String(), "cancelling...") {
		t.Errorf("output %q", buf.String())
	}
}

func TestSpinnerGlyphsFallBackToASCII(t *testing.T) {
	src := &fakeSource{s: countedSnap("records.stream", 1, 10, "records", time.Second)}
	var buf bytes.Buffer
	tick := make(chan time.Time)
	stop := startStatus(&buf, src, tick, func() time.Time { return statusNow }, nil, false, 0)
	for range 12 {
		tick <- statusNow
		src.bumpSeq()
	}
	tick <- statusNow
	stop()
	if buf.Len() == 0 {
		t.Fatal("nothing drawn")
	}
	for i, b := range buf.Bytes() {
		if b >= 0x80 {
			t.Fatalf("non-ASCII byte at %d in %q", i, buf.String())
		}
	}
}

func TestOutcomeLine(t *testing.T) {
	problems := evidence.VerifyReport{Problems: []string{"a"}}
	two := evidence.VerifyReport{Problems: []string{"a", "b"}}
	cancelled := evidence.VerifyReport{Problems: []string{"reproduce: verification cancelled"}}
	if !cancelled.CancelledOnly() {
		t.Fatal("the cancelled fixture is not CancelledOnly; update the test")
	}
	cases := []struct {
		name string
		rep  evidence.VerifyReport
		err  error
		want string
		ok   bool
	}{
		{"ok", evidence.VerifyReport{}, nil, "verify finished: OK", true},
		{"one", problems, nil, "verify finished: 1 problem (exit 4)", true},
		{"many", two, nil, "verify finished: 2 problems (exit 4)", true},
		{"cancelled", cancelled, nil, "verify cancelled (exit 1)", true},
		{"integrity", evidence.VerifyReport{}, fmt.Errorf("x: %w", evidence.ErrIntegrity), "verify failed: integrity (exit 4)", true},
		{"other", evidence.VerifyReport{}, errors.New("x"), "", false},
	}
	for _, c := range cases {
		got, ok := outcomeLine(c.rep, c.err)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: outcomeLine = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestIsTerminalFalseForPipeAndBuffer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if errIsTerminal(Deps{Err: w}) || errIsTerminal(Deps{Err: &bytes.Buffer{}}) || errIsTerminal(Deps{}) {
		t.Error("a pipe, a buffer or nil was taken for a terminal")
	}
	if !errIsTerminal(Deps{Err: &bytes.Buffer{}, ErrTerminal: TerminalOn}) {
		t.Error("TerminalOn did not force")
	}
	if errIsTerminal(Deps{Err: w, ErrTerminal: TerminalOff}) {
		t.Error("TerminalOff did not force")
	}
}

func TestLocaleUTF8(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		m    map[string]string
		want bool
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, true},
		{map[string]string{"LANG": "en_US.utf8"}, true},
		{map[string]string{"LANG": "C"}, false},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{nil, false},
	} {
		if got := localeUTF8(env(c.m)); got != c.want {
			t.Errorf("%v: %v", c.m, got)
		}
	}
}

func TestStatusFramesAreCutToTheTerminalWidth(t *testing.T) {
	src := &fakeSource{s: countedSnap("fts.compare/records_fts_trigram/vocabulary-merge", 123456789, 987654321, "records", time.Hour)}
	var buf bytes.Buffer
	tick := make(chan time.Time)
	stop := startStatus(&buf, src, tick, func() time.Time { return statusNow }, nil, true, 30)
	tick <- statusNow
	tick <- statusNow
	stop()
	frames := strings.Split(buf.String(), "\r")
	drawn := 0
	for _, f := range frames {
		if n := len([]rune(f)); n > 29 {
			t.Errorf("frame %q is %d runes wide, want at most 29 on a 30-column console", f, n)
		}
		if strings.Contains(f, "fts.compare") {
			drawn++
		}
	}
	if drawn == 0 {
		t.Fatalf("nothing drawn: %q", buf.String())
	}
}
