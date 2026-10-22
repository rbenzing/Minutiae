package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// The status line is a read-only view of a verify in progress. It states only what was observed: the
// counters moved, or they did not move for so long. It never says the run is hung or healthy.
const (
	stallCounted   = 60 * time.Second // no counter movement in a counted phase
	stallUncounted = 10 * time.Minute // no phase change in a phase without a counter
)

// statusInterval is the refresh period of the status line (a variable so tests can run it fast).
var statusInterval = time.Second

var (
	unicodeSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	asciiSpinner   = []string{"|", "/", "-", `\`}
)

type progressSource interface {
	Snapshot() evidence.VerifyProgressSnapshot
}

// statusState is what the renderer remembers between refreshes.
type statusState struct {
	seen     bool
	lastSeq  uint64
	lastMove time.Time // when Seq last changed
	spin     int
}

// renderStatus draws one frame. The spinner advances only when Seq changed since the last frame, so a
// frozen run shows a frozen glyph.
func renderStatus(s evidence.VerifyProgressSnapshot, st *statusState, now time.Time, cancelling, unicode bool) string {
	switch {
	case !st.seen:
		st.seen, st.lastSeq, st.lastMove = true, s.Seq, now
	case s.Seq != st.lastSeq:
		st.lastSeq, st.lastMove = s.Seq, now
		st.spin++
	}
	frames := asciiSpinner
	if unicode {
		frames = unicodeSpinner
	}
	spin := frames[st.spin%len(frames)]
	since := now.Sub(st.lastMove)
	if since < 0 {
		since = 0
	}

	if cancelling {
		dots := "..."
		if unicode {
			dots = "…"
		}
		line := spin + " cancelling" + dots
		if s.Phase != "" {
			line += "  " + s.Phase
		}
		return line
	}
	if s.Phase == "" {
		return fmt.Sprintf("%s other checks  elapsed %s", spin, dur(since))
	}
	elapsed := now.Sub(s.Started)
	if elapsed < 0 {
		elapsed = 0
	}
	if !s.Counted {
		line := fmt.Sprintf("%s %s  elapsed %s  ", spin, s.Phase, dur(elapsed))
		if since >= stallUncounted {
			return line + "no progress for " + dur(since)
		}
		return line + "no inner progress for this phase"
	}

	line := fmt.Sprintf("%s %s  ", spin, s.Phase)
	stalled := since >= stallCounted
	var tail string
	switch {
	case s.Total < 0:
		line += fmt.Sprintf("%s %s (total unknown)", group(s.Done), s.Unit)
	case s.Done > s.Total:
		line += fmt.Sprintf("%s %s (more than the %s audited)", group(s.Done), s.Unit, group(s.Total))
	default:
		line += fmt.Sprintf("%s / %s %s", group(s.Done), group(s.Total), s.Unit)
		if !stalled && s.Done > 0 && elapsed >= time.Second {
			rate := float64(s.Done) / elapsed.Seconds()
			if rate > 0 {
				eta := time.Duration(float64(s.Total-s.Done) / rate * float64(time.Second))
				tail = "  ETA " + dur(eta)
			}
		}
	}
	if stalled {
		return line + "  no progress for " + dur(since)
	}
	if s.Done > 0 && elapsed >= time.Second {
		line += fmt.Sprintf("  %s/s", group(int64(float64(s.Done)/elapsed.Seconds()+0.5)))
	}
	return line + tail + fmt.Sprintf("  (last progress %s ago)", dur(since))
}

// dur is a duration rounded to whole seconds.
func dur(d time.Duration) string { return d.Round(time.Second).String() }

// group writes n with commas between the thousands.
func group(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// startStatus draws a frame on w at every tick until stop is called. stop returns only after the loop has
// exited and, when something was drawn, has cleared the line, so it can run before any stdout output.
func startStatus(w io.Writer, src progressSource, tick <-chan time.Time, now func() time.Time, cancelled <-chan struct{}, unicode bool, cols int) (stop func()) {
	if cols < 2 {
		cols = defaultStatusWidth
	}
	quit := make(chan struct{})
	exited := make(chan struct{})
	width := 0
	st := &statusState{}
	draw := func() {
		isCancelling := false
		select {
		case <-cancelled: // a nil channel never fires
			isCancelling = true
		default:
		}
		line := fitWidth(renderStatus(src.Snapshot(), st, now(), isCancelling, unicode), cols-1) // never the last column: no wrap
		n := utf8.RuneCountInString(line)
		pad := ""
		if n < width {
			pad = strings.Repeat(" ", width-n)
		}
		_, _ = io.WriteString(w, ""+line+pad)
		width = max(width, n)
	}
	draw() // the first frame is drawn now, not a tick later
	go func() {
		defer close(exited)
		for {
			select {
			case <-quit:
				return
			case <-tick:
			}
			draw()
		}
	}()
	return func() {
		close(quit)
		<-exited
		if width > 0 {
			_, _ = io.WriteString(w, "\r"+strings.Repeat(" ", width)+"\r")
		}
	}
}

// outcomeLine is the final stderr line of a verify on a terminal. ok is false for an error that keeps
// today's output (no extra line).
func outcomeLine(rep evidence.VerifyReport, err error) (string, bool) {
	if err != nil {
		if errors.Is(err, evidence.ErrIntegrity) {
			return "verify failed: integrity (exit 4)", true
		}
		return "", false
	}
	switch {
	case rep.OK():
		return "verify finished: OK", true
	case rep.CancelledOnly():
		return "verify cancelled (exit 1)", true
	case len(rep.Problems) == 1:
		return "verify finished: 1 problem (exit 4)", true
	default:
		return fmt.Sprintf("verify finished: %d problems (exit 4)", len(rep.Problems)), true
	}
}

// fitWidth cuts line to at most n runes.
func fitWidth(line string, n int) string {
	if utf8.RuneCountInString(line) <= n {
		return line
	}
	return string([]rune(line)[:max(n, 0)])
}
