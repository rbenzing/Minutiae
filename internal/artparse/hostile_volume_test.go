package artparse_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
	"github.com/rbenzing/minutiae/internal/records"
)

// Audit volume on hostile inputs (4H, 4h-R12). Every registered parser runs through the real host over
// four deterministic mutations of its largest committed fixture database. The mutations are pure
// functions of the file's bytes and the page size read from its header. No mutation may panic a job,
// lose a warning, reach the per-ingest warning cap or write an audit line over hostileLineBound bytes.

// hostileLineBound is the largest audit line (bytes, newline included) a run may write (4h-R12).
const hostileLineBound = 16 << 10

type hostileMutation struct {
	name  string
	apply func(db []byte, pageSize int) []byte
}

func hostileMutations() []hostileMutation {
	return []hostileMutation{
		{"zero-middle-page", func(b []byte, ps int) []byte {
			n := len(b) / ps
			if n < 2 { // a one-page file: everything after the 100-byte file header
				clear(b[100:min(ps, len(b))])
				return b
			}
			clear(b[(n/2)*ps : (n/2+1)*ps])
			return b
		}},
		{"root-cell-count-max", func(b []byte, _ int) []byte {
			// the root page of the schema (page 1): its b-tree header starts at byte 100, the cell count is bytes 3-4 of it
			binary.BigEndian.PutUint16(b[103:], 0xFFFF)
			return b
		}},
		{"cell-pointer-past-page", func(b []byte, ps int) []byte {
			// the first b-tree page after page 1 that holds a cell: its first cell pointer points at the page size,
			// past the last byte of the page
			for p := 1; p < len(b)/ps; p++ {
				h := p * ps
				hdr := 8
				switch b[h] {
				case 0x0d:
				case 0x05:
					hdr = 12
				default:
					continue
				}
				if binary.BigEndian.Uint16(b[h+3:]) == 0 {
					continue
				}
				binary.BigEndian.PutUint16(b[h+hdr:], uint16(min(ps, 0xFFFF)))
				return b
			}
			binary.BigEndian.PutUint16(b[100+8:], uint16(min(ps, 0xFFFF))) // no other page holds a cell: page 1
			return b
		}},
		{"truncate-60-percent", func(b []byte, _ int) []byte { return b[:len(b)*6/10] }},
	}
}

func hostilePageSize(t *testing.T, b []byte) int {
	t.Helper()
	if len(b) < 108 || !bytes.HasPrefix(b, []byte("SQLite format 3\x00")) {
		t.Fatal("the fixture is not a SQLite database")
	}
	ps := int(binary.BigEndian.Uint16(b[16:]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		t.Fatalf("page size %d", ps)
	}
	return ps
}

// largestFixture returns the name and decompressed bytes of the largest <name>.db.gz of dir (a parser's
// images database is not its primary).
func largestFixture(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "testdata", "*.db.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in %s: %v", dir, err)
	}
	var bestName string
	var best []byte
	for _, p := range paths {
		base := filepath.Base(p)
		if strings.Contains(base, ".images.") {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(zr)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > len(best) || (len(b) == len(best) && base < bestName) {
			bestName, best = base, b
		}
	}
	return bestName, best
}

// hostilePath is a device path the parser's first primary glob matches, and the acquisition type that
// makes the device's platform known.
func hostilePath(t *testing.T, m parse.Meta) (remote, acqType string) {
	t.Helper()
	g := m.Inputs[0].Globs[0]
	switch {
	case strings.HasPrefix(g, "android:"):
		g = strings.NewReplacer("**", "data/data/pkg", "*", "x").Replace(strings.TrimPrefix(g, "android:"))
		return "/" + strings.TrimPrefix(g, "/"), "logical"
	case strings.HasPrefix(g, "ios:*/"):
		return "/" + strings.TrimPrefix(g, "ios:*/"), "ios.backup"
	}
	t.Fatalf("%s: glob %q", m.Name, g)
	return "", ""
}

// hostileRegistry is the registry as internal/cli registers it.
func hostileRegistry(t *testing.T) []artparse.Registered {
	t.Helper()
	var out []artparse.Registered
	for _, p := range parsers.All() {
		hash, ok := parsers.HashOf(p)
		if !ok {
			t.Fatalf("no hash for %T", p)
		}
		r, err := artparse.Register(p, hash)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Meta().Claims) > 0 {
			rh, ok := parsers.RecoveryHashOf(p)
			if !ok {
				t.Fatalf("%s: no recovery hash", r.Meta().Name)
			}
			if r, err = r.WithRecoveryHash(rh); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, r)
	}
	return out
}

// hostileDirs maps a parser name to its package directory under internal/parsers.
var hostileDirs = map[string]string{
	"android.mmssms": "androidmms", "android.calllog": "androidcalllog", "android.contacts": "androidcontacts",
	"ios.sms": "iossms", "ios.calls": "ioscalls", "ios.addressbook": "iosaddressbook",
	"chromium.history": "chromium", "safari.history": "safari",
}

// hostileVolume is what one job wrote and what the log proves about its warnings.
type hostileVolume struct {
	longest    int // the longest line of the whole log
	bytes      int // bytes of the entries from parse.job.start to parse.job.end
	entries    int // those entries
	jobWarn    int // parse.job.end warnings
	jobSupp    int // parse.job.end warnings_suppressed
	tallyWarn  int // analysis.warning entries naming the ingest (not the suppression notes)
	tallySupp  int // what the suppression note says the cap kept out
	noteCounts int // suppression notes (at most one)
}

func hostileCount(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case int:
		return x
	}
	return 0
}

// measureHostileJob reads the case's audit log file and reports what the job between its parse.job.start
// and parse.job.end appended; the line lengths are those of the file itself.
func measureHostileJob(t *testing.T, c *evidence.Case, ingestID string) hostileVolume {
	t.Helper()
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != len(entries) {
		t.Fatalf("%d lines, %d entries", len(lines), len(entries))
	}
	var v hostileVolume
	inJob := false
	for i, e := range entries {
		v.longest = max(v.longest, len(lines[i]))
		switch e.Action {
		case artparse.ActionParseJobStart:
			inJob = true
		case artparse.ActionParseJobEnd:
			end := decodeEntry[artparse.JobEnd](t, e)
			v.jobWarn, v.jobSupp = end.Warnings, end.WarningsSuppressed
			v.entries++
			v.bytes += len(lines[i])
			inJob = false
			continue
		case evidence.ActionAnalysisWarning:
			if e.Details[evidence.WarnKeyIngest] != ingestID {
				break
			}
			switch {
			case e.Details[evidence.WarnKeySuppressionBegan] == true:
			case e.Details[evidence.WarnKeySuppression] == true:
				v.noteCounts++
				v.tallySupp += hostileCount(e.Details[evidence.WarnKeySuppressedWarnings]) + hostileCount(e.Details[evidence.WarnKeySuppressedRejects])
			default:
				v.tallyWarn++
			}
		}
		if inJob {
			v.entries++
			v.bytes += len(lines[i])
		}
	}
	return v
}

func TestHostileAuditVolumeIsBounded(t *testing.T) {
	type maxima struct{ warnings, longest, bytes int }
	maxOf := map[string]*maxima{}
	var order []string
	for _, reg := range hostileRegistry(t) {
		meta := reg.Meta()
		dir, ok := hostileDirs[meta.Name]
		if !ok {
			t.Fatalf("parser %s has no fixture directory in hostileDirs", meta.Name)
		}
		fixName, fixture := largestFixture(t, filepath.Join("..", "parsers", dir))
		remote, acqType := hostilePath(t, meta)
		maxOf[meta.Name] = &maxima{}
		order = append(order, meta.Name)
		for _, mut := range hostileMutations() {
			t.Run(meta.Name+"/"+mut.name, func(t *testing.T) {
				data := mut.apply(bytes.Clone(fixture), hostilePageSize(t, fixture))
				if bytes.Equal(data, fixture) {
					t.Fatalf("mutation %s leaves %s unchanged", mut.name, fixName)
				}
				c := newCase(t)
				acquireStart(t, c, "D1", "A1", acqType)
				putFile(t, c, "D1", "A1", remote, string(data))
				h, err := artparse.New(c, hostileRegistry(t), artparse.Options{Limits: parse.DefaultLimits()})
				if err != nil {
					t.Fatal(err)
				}
				sum, err := h.Run(context.Background(), artparse.RunOptions{Selection: artparse.Selection{Parsers: []artparse.ParserRef{{Name: meta.Name}}}})
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if len(sum.Jobs) != 1 || sum.Jobs[0].Parser != meta.Name {
					t.Fatalf("jobs %+v, want exactly one of %s", sum.Jobs, meta.Name)
				}
				j := sum.Jobs[0]
				if j.Panic != nil || j.Abandoned || j.TimedOut || j.Integrity {
					t.Fatalf("outcome %s reason %q: panic %v abandoned %v timed out %v integrity %v", j.Outcome, j.Reason, j.Panic, j.Abandoned, j.TimedOut, j.Integrity)
				}
				if lim := parse.DefaultLimits().MaxRejected; j.Rejected > lim {
					t.Errorf("rejected %d over MaxRejected %d", j.Rejected, lim)
				}
				v := measureHostileJob(t, c, j.IngestID)
				if v.tallyWarn != v.jobWarn {
					t.Errorf("the log holds %d analysis.warning entries for the ingest, parse.job.end says %d", v.tallyWarn, v.jobWarn)
				}
				if v.tallySupp != v.jobSupp || j.WarningsSuppressed != v.jobSupp {
					t.Errorf("the log proves %d suppressed entries, parse.job.end says %d, the summary %d", v.tallySupp, v.jobSupp, j.WarningsSuppressed)
				}
				total := v.tallyWarn + v.tallySupp
				if total >= records.MaxWarningsPerIngest {
					t.Errorf("%d warnings (%d written, %d suppressed) reach the per-ingest cap %d: one mutation must not (4h-R12: STOP, NEEDS_CONTEXT)", total, v.tallyWarn, v.tallySupp, records.MaxWarningsPerIngest)
				}
				if v.longest > hostileLineBound {
					t.Errorf("longest audit line %d bytes, bound %d", v.longest, hostileLineBound)
				}
				if v.bytes > v.entries*hostileLineBound {
					t.Errorf("job appended %d bytes in %d entries, bound %d per entry", v.bytes, v.entries, hostileLineBound)
				}
				m := maxOf[meta.Name]
				m.warnings, m.longest, m.bytes = max(m.warnings, total), max(m.longest, v.longest), max(m.bytes, v.bytes)
				t.Logf("%s: outcome %s, warnings %d (suppressed %d), rejected %d, longest line %d, bytes %d in %d entries", mut.name, j.Outcome, total, v.tallySupp, j.Rejected, v.longest, v.bytes, v.entries)
			})
		}
	}
	for _, name := range order {
		m := maxOf[name]
		t.Logf("%-18s max warnings/job %d, longest line %d, max bytes/job %d", name, m.warnings, m.longest, m.bytes)
	}
}
