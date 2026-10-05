package parsertest

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

// canonicalTime is the stable form of a record time: Unix microseconds (what
// the case stores), the basis and the offset.
func canonicalTime(t *records.Time) any {
	if t == nil {
		return nil
	}
	return map[string]any{"us": t.T.UnixMicro(), "basis": string(t.Basis), "offset_min": t.OffsetMin}
}

// CanonicalRecords returns one JSON line per record with sorted keys (map keys
// are sorted by encoding/json at every level), so two runs can be compared as
// text.
func CanonicalRecords(rs []records.Record) []string {
	out := make([]string, 0, len(rs))
	for i, r := range rs {
		times := make([]any, len(r.Times))
		for j, nt := range r.Times {
			times[j] = map[string]any{"kind": nt.Kind, "time": canonicalTime(&nt.Time)}
		}
		var rng any
		if r.Range != nil {
			rng = map[string]any{"offset": r.Range.Offset, "length": r.Range.Length}
		}
		var conf any
		if r.Confidence != nil {
			conf = *r.Confidence
		}
		line, err := json.Marshal(map[string]any{
			"type": r.Type, "artifact_id": r.ArtifactID, "source_path": r.SourcePath, "locator": r.Locator,
			"range": rng, "time": canonicalTime(r.Time), "time_end": canonicalTime(r.TimeEnd), "times": times,
			"deleted": r.Deleted, "recovery": r.Recovery, "confidence": conf,
			"summary": r.Summary, "body": r.Body, "payload": r.Payload,
		})
		if err != nil {
			line = []byte(fmt.Sprintf("record %d: cannot be encoded: %v", i, err))
		}
		out = append(out, string(line))
	}
	return out
}

// AssertDeterministic calls newRun under four configurations and requires the
// same canonical record sequence from all of them: the default; GOMAXPROCS(1);
// the default again; and with time.Local set to a fixed zone that differs from
// the machine's, so a parser that depends on the host time zone fails here even
// though the purity rule already bans it. GOMAXPROCS and time.Local are
// restored afterwards, also when a run fails the test. newRun must reuse the
// same artifacts every time (a new case gives new artifact ids, which are part
// of a record).
func AssertDeterministic(t testing.TB, newRun func() Result) {
	t.Helper()
	procs, local := runtime.GOMAXPROCS(0), time.Local
	defer func() {
		runtime.GOMAXPROCS(procs)
		time.Local = local
	}()
	configs := []struct {
		name  string
		apply func()
	}{
		{"default", func() {}},
		{"GOMAXPROCS(1)", func() { runtime.GOMAXPROCS(1) }},
		{"default again", func() {}},
		{"other time zone", func() { time.Local = otherZone(local) }},
	}
	var want []string
	for i, c := range configs {
		runtime.GOMAXPROCS(procs)
		time.Local = local
		c.apply()
		got := CanonicalRecords(newRun().Records)
		if i == 0 {
			want = got
			continue
		}
		if len(got) != len(want) {
			t.Errorf("parsertest: %s: %d records, the default run gave %d", c.name, len(got), len(want))
			continue
		}
		for j := range got {
			if got[j] != want[j] {
				t.Errorf("parsertest: %s: record %d differs from the default run:\n got %s\nwant %s", c.name, j, got[j], want[j])
				break
			}
		}
	}
}

// otherZone returns a fixed zone whose offset differs from local's now and at
// a mid-year date, so a zone-dependent rendering changes.
func otherZone(local *time.Location) *time.Location {
	for _, secs := range []int{13*3600 + 45*60, -(3*3600 + 30*60), 5*3600 + 30*60} {
		same := false
		for _, at := range []time.Time{time.Unix(1_700_000_000, 0), time.Unix(1_720_000_000, 0), time.Now()} {
			if _, off := at.In(local).Zone(); off == secs {
				same = true
			}
		}
		if !same {
			return time.FixedZone("parsertest/other", secs)
		}
	}
	return time.FixedZone("parsertest/other", 12*3600+1)
}
