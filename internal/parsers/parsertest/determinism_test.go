package parsertest

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// zoneParser embeds the host's time zone in its payload, the mistake the
// time.Local configuration of AssertDeterministic exists to catch.
type zoneParser struct{ WellBehaved }

func (p zoneParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	r := validRecord(in, 1)
	r.Body = time.Unix(1_700_000_000, 0).String()
	return out.Emit(ctx, r)
}

func TestParsersAreDeterministic(t *testing.T) {
	h, art, _ := bundle(t, t, "det", "one\ntwo\nthree\n")
	run := func(p parse.Parser) func() Result {
		return func() Result { return h.Run(p, h.Input(p, art, nil, false)) }
	}

	AssertDeterministic(t, run(WellBehaved{Name: "det", Version: "1.0.0"}))

	for name, p := range map[string]parse.Parser{
		"math/rand": Nondeterministic{Name: "det", Version: "1.0.0"},
		"host zone": zoneParser{WellBehaved{Name: "det", Version: "1.0.0"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{TB: t}
			AssertDeterministic(rec, run(p))
			if !rec.Failed() {
				t.Error("AssertDeterministic accepted a nondeterministic parser")
			}
		})
	}
}

func TestAssertDeterministicRunsFourConfigurationsAndRestores(t *testing.T) {
	procs, local := runtime.GOMAXPROCS(0), time.Local
	var seenProcs []int
	var seenZones []string
	AssertDeterministic(t, func() Result {
		seenProcs = append(seenProcs, runtime.GOMAXPROCS(0))
		seenZones = append(seenZones, time.Local.String())
		return Result{}
	})
	if len(seenProcs) != 4 {
		t.Fatalf("newRun was called %d times, want 4", len(seenProcs))
	}
	if seenProcs[1] != 1 || seenProcs[0] != procs || seenProcs[2] != procs {
		t.Errorf("GOMAXPROCS seen %v (machine %d)", seenProcs, procs)
	}
	if seenZones[3] == seenZones[0] || seenZones[0] != seenZones[1] || seenZones[0] != seenZones[2] {
		t.Errorf("time.Local seen %v", seenZones)
	}
	if runtime.GOMAXPROCS(0) != procs || time.Local != local {
		t.Error("GOMAXPROCS or time.Local was not restored")
	}

	// restored after a failure too
	rec := &recorder{TB: t}
	n := 0
	AssertDeterministic(rec, func() Result {
		n++
		return Result{Notes: map[string]string{"n": strings.Repeat("x", n)}, Records: []records.Record{{Type: "message", Summary: strings.Repeat("y", n)}}}
	})
	if !rec.Failed() || runtime.GOMAXPROCS(0) != procs || time.Local != local {
		t.Errorf("failed=%v procs=%d local restored=%v", rec.Failed(), runtime.GOMAXPROCS(0), time.Local == local)
	}
}

func TestCanonicalRecords(t *testing.T) {
	tm := records.Time{T: time.Date(2024, 1, 2, 3, 4, 5, 6000, time.UTC), Basis: records.BasisLocalOffset, OffsetMin: 60}
	r := records.Record{
		Type: "message", ArtifactID: "a1", Locator: "text:line=1", Range: &records.Range{Offset: 1, Length: 2},
		Time: &tm, Times: []records.NamedTime{{Kind: "sent", Time: tm}}, Summary: "s",
		Payload: map[string]any{"b": 1, "a": map[string]any{"z": true, "y": []any{"q"}}},
	}
	lines := CanonicalRecords([]records.Record{r, r})
	if len(lines) != 2 || lines[0] != lines[1] || strings.Contains(lines[0], "\n") {
		t.Fatalf("lines %q", lines)
	}
	if !strings.Contains(lines[0], `"a":{"y":["q"],"z":true},"b":1`) {
		t.Errorf("payload keys are not sorted: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"artifact_id":"a1"`) || strings.Index(lines[0], `"artifact_id"`) > strings.Index(lines[0], `"type"`) {
		t.Errorf("record keys are not sorted: %s", lines[0])
	}
	// a different range, time or payload gives a different line
	for name, mod := range map[string]func(*records.Record){
		"range":   func(r *records.Record) { r.Range = &records.Range{Offset: 1, Length: 3} },
		"time":    func(r *records.Record) { v := tm; v.T = v.T.Add(time.Microsecond); r.Time = &v },
		"basis":   func(r *records.Record) { v := tm; v.Basis = records.BasisUTC; r.Time = &v },
		"payload": func(r *records.Record) { r.Payload = map[string]any{"a": 1} },
		"deleted": func(r *records.Record) { r.Deleted = true },
	} {
		c := r
		mod(&c)
		if CanonicalRecords([]records.Record{c})[0] == lines[0] {
			t.Errorf("a change of %s does not show in the canonical line", name)
		}
	}
}
