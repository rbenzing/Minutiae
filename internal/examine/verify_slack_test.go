package examine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// ---- helpers -------------------------------------------------------------------------------------------

func verifyAll(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), VerifyChecks(Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func verifySlackOnly(ctx context.Context, t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(ctx, SlackCheck(nil))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func slackProblems(rep evidence.VerifyReport) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, "slack:") {
			out = append(out, p)
		}
	}
	return out
}

func requireSlackProblem(t *testing.T, rep evidence.VerifyReport, substr string) {
	t.Helper()
	ps := slackProblems(rep)
	for _, p := range ps {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no slack problem contains %q; slack problems:\n%s", substr, strings.Join(ps, "\n"))
}

func requireNoSlackProblem(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	if ps := slackProblems(rep); len(ps) != 0 {
		t.Fatalf("unexpected slack problems:\n%s", strings.Join(ps, "\n"))
	}
}

// forgeEnv is a FAT16 image with five files and a volume tail, imported into a case: A, B and C have a
// non-uniform tail, D fits its clusters exactly and Z has a zero tail.
type forgeEnv struct {
	f       *slackFAT
	img     []byte
	s       *Session
	c       *evidence.Case
	cs      int64
	file    []SlackSegment // plan.file with IncludeZero
	vol     []SlackSegment
	fsID    map[string]string // file name -> fs_id
	exactID string            // fs_id of D.TXT, whose size fills its clusters
}

func newForgeEnv(t *testing.T) *forgeEnv {
	t.Helper()
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(1)},
		{Path: "B.TXT", Data: slackFill(cs - 5)},
		{Path: "D.TXT", Data: slackFill(cs)}, // exact fit: no slack
		{Path: "Z.TXT", Data: slackFill(10)},
		{Path: "C.TXT", Data: slackFill(cs + 3)},
	}, 2)
	f.patchTail("A.TXT", false, 1)
	f.patchTail("B.TXT", false, cs-5)
	f.patchTail("C.TXT", false, cs+3)
	img := slices.Concat(f.img, pat(8192, 3))
	c := newSlackCase(t)
	s := slackSessionOn(t, c, img, Options{})
	plan := slackPlanOf(t, s, SlackOptions{Partition: -1, IncludeZero: true})
	e := &forgeEnv{f: f, img: img, s: s, c: c, cs: cs, file: plan.file, vol: plan.vol, fsID: map[string]string{}}
	for _, g := range plan.file {
		e.fsID[strings.TrimPrefix(g.FSPath, "/")] = g.FSID
	}
	d := f.built.Find("D.TXT", false)
	e.exactID = fmt.Sprintf("dirent:%d:%d", d.DirCluster, d.Index)
	if len(e.file) != 4 || len(e.vol) != 1 {
		t.Fatalf("setup: %d file and %d volume segments", len(e.file), len(e.vol))
	}
	return e
}

func lineOf(g SlackSegment) evidence.SlackMapLine {
	return evidence.SlackMapLine{
		ImageOffset: g.ImageOffset, Length: g.Length, FSPath: g.FSPath, FSID: g.FSID, PathEscaped: g.PathEscaped,
		FileSize: g.FileSize, Uniform: g.Uniform, Exported: true,
	}
}

func volLineOf(g SlackSegment) evidence.SlackMapLine {
	return evidence.SlackMapLine{ImageOffset: g.ImageOffset, Length: g.Length, Uniform: g.Uniform, Exported: true}
}

func (e *forgeEnv) fileLines() []evidence.SlackMapLine {
	var out []evidence.SlackMapLine
	for _, g := range e.file {
		out = append(out, lineOf(g))
	}
	return out
}

// add stores an incomplete artifact (so that the per-segment checks name each forgery) of partition 0.
func (e *forgeEnv) add(t *testing.T, lines []evidence.SlackMapLine, mut func(*evidencetest.SlackSpec)) (bin, side evidence.ManifestRecord) {
	t.Helper()
	spec := evidencetest.SlackSpec{
		Partition0: true, FSType: "fat16", Lines: lines, Incomplete: true,
		Params: map[string]string{"include_zero": "true"},
	}
	if mut != nil {
		mut(&spec)
	}
	return evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, spec)
}

// ---- tests ---------------------------------------------------------------------------------------------

func TestVerifySlackReproduces(t *testing.T) {
	f, img := fullFAT(t)
	_ = f
	s := slackSession(t, img, Options{})
	sum := mustExportSlack(t, s, SlackOptions{Partition: -1})
	if sum.Incomplete || sum.Files != 2 {
		t.Fatalf("setup: summary %+v", sum)
	}
	rep := verifyAll(t, s.Case)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredReproduced != 2 || rep.SlackRechecked != 2 {
		t.Errorf("RecoveredReproduced = %d, SlackRechecked = %d, want 2 and 2", rep.RecoveredReproduced, rep.SlackRechecked)
	}
	for _, n := range rep.Notices {
		if strings.Contains(n, `kind "slack"`) || strings.Contains(n, "slack artifact") {
			t.Errorf("unexpected notice: %s", n)
		}
	}
	// the include-zero form is re-derived too
	z, _ := fatCutSlack(t, false)
	s2 := slackSession(t, slices.Concat(z.img, pat(4096, 5)), Options{})
	mustExportSlack(t, s2, SlackOptions{Partition: -1, IncludeZero: true, Files: true})
	r2 := verifyAll(t, s2.Case)
	evidencetest.RequireClean(t, r2)
}

func TestVerifySlackTamperIsAProblem(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(size int64) int64
	}{
		{"first", func(int64) int64 { return 0 }},
		{"middle", func(n int64) int64 { return n / 2 }},
		{"last", func(n int64) int64 { return n - 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, img := fullFAT(t)
			s := slackSession(t, img, Options{})
			sum := mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
			bin := slackRecord(t, sum, "p0-fat16/slack.bin")
			off := tc.at(bin.Size)
			evidencetest.FlipArtifactByte(t, s.Case.Dir, bin, off)
			rep := verifyAll(t, s.Case)
			evidencetest.RequireProblem(t, rep, fmt.Sprintf("reproduce: byte %d of the artifact differs", off))
		})
	}
}

func TestVerifySlackRecheckFindsForgedSegments(t *testing.T) {
	t.Run("the genuine lines are clean", func(t *testing.T) {
		e := newForgeEnv(t)
		e.add(t, e.fileLines(), nil)
		_, _ = evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{
			Partition0: true, FSType: "fat16", Method: evidence.MethodSlackVolume, Incomplete: true,
			Lines: []evidence.SlackMapLine{volLineOf(e.vol[0])}, Params: map[string]string{"include_zero": "true"},
		})
		rep := verifySlackOnly(context.Background(), t, e.c)
		requireNoSlackProblem(t, rep)
		if rep.SlackRechecked != 2 {
			t.Errorf("SlackRechecked = %d, want 2", rep.SlackRechecked)
		}
	})

	type forge struct {
		name   string
		lines  func(e *forgeEnv) []evidence.SlackMapLine
		mut    func(*evidencetest.SlackSpec)
		want   string
		volume bool
	}
	cases := []forge{
		{name: "an offset one byte early", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].ImageOffset--
			return l
		}, want: "is not the tail slack of the file"},
		{name: "a length one byte short", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].Length--
			return l
		}, want: "is not the tail slack of the file"},
		{name: "the fs_id of an exact-fit file", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].FSID, l[0].FileSize = e.exactID, e.cs
			return l
		}, want: "the file has no tail slack now (exact-fit)"},
		{name: "an unknown fs_id", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].FSID = "dirent:7777:3"
			return l
		}, want: "the file cannot be opened"},
		{name: "a wrong recorded size", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].FileSize++
			return l
		}, want: "the file is 1 bytes now, the segment records 2"},
		{name: "a segment inside free space", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			// the last cluster of the data area is free in this image
			l[0].ImageOffset = int64(len(e.f.img)) - e.cs
			l[0].Length = 16
			return l
		}, want: "lies in space the filesystem reports unallocated"},
		{name: "two overlapping segments", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			return append(l, l[0])
		}, want: "segments 0 and 4 overlap in the image"},
		{name: "uniform false over zero bytes", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			for i := range l {
				if l[i].FSPath == "/Z.TXT" {
					l[i].Uniform = false
				}
			}
			return l
		}, want: "records uniform false, but its bytes in the artifact are uniform"},
		{name: "uniform true over data", lines: func(e *forgeEnv) []evidence.SlackMapLine {
			l := e.fileLines()
			l[0].Uniform = true
			return l
		}, want: "records uniform true, but its bytes in the artifact are not uniform"},
		{
			name: "uniform true without include_zero", lines: func(e *forgeEnv) []evidence.SlackMapLine { return e.fileLines() },
			mut:  func(s *evidencetest.SlackSpec) { s.Params = map[string]string{"include_zero": "false"} },
			want: "is uniform but include_zero is not set",
		},
		{name: "a volume segment inside the filesystem", volume: true, lines: func(*forgeEnv) []evidence.SlackMapLine {
			return []evidence.SlackMapLine{{ImageOffset: 0, Length: 4096, Uniform: false, Exported: true}}
		}, want: "lies inside the filesystem"},
		{name: "a volume segment past the partition end", volume: true, lines: func(e *forgeEnv) []evidence.SlackMapLine {
			v := volLineOf(e.vol[0])
			v.ImageOffset += 100
			return []evidence.SlackMapLine{v}
		}, want: "lies past the end of the partition"},
		{
			name: "a recorded partition offset that differs", lines: func(e *forgeEnv) []evidence.SlackMapLine { return e.fileLines() },
			mut:  func(s *evidencetest.SlackSpec) { s.PartitionOffset = 512 },
			want: "partition 0 starts at 0, the artifact records 512",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newForgeEnv(t)
			mut := tc.mut
			if tc.volume {
				e.addVol(t, tc.lines(e))
			} else {
				e.add(t, tc.lines(e), mut)
			}
			rep := verifySlackOnly(context.Background(), t, e.c)
			requireSlackProblem(t, rep, tc.want)
			if rep.SlackRechecked != 0 {
				t.Errorf("SlackRechecked = %d for a forged artifact", rep.SlackRechecked)
			}
		})
	}
	t.Run("the same forgery as a complete artifact is a re-plan mismatch", func(t *testing.T) {
		e := newForgeEnv(t)
		l := e.fileLines()
		l[0].ImageOffset--
		e.add(t, l, func(s *evidencetest.SlackSpec) { s.Incomplete = false })
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "segment 0 differs from the re-plan: image_offset is")
	})
}

func (e *forgeEnv) addVol(t *testing.T, lines []evidence.SlackMapLine) {
	t.Helper()
	evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{
		Partition0: true, FSType: "fat16", Method: evidence.MethodSlackVolume, Incomplete: true, Lines: lines,
		Params: map[string]string{"include_zero": "true"},
	})
}

func TestVerifySlackNotComposedNotice(t *testing.T) {
	_, img := fullFAT(t)
	s := slackSession(t, img, Options{})
	mustExportSlack(t, s, SlackOptions{Partition: -1})
	plain, err := s.Case.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNotice(t, plain, "2 slack artifact(s): segments not re-derived from the filesystem (this verify did not compose check R11)")
	evidencetest.RequireNotice(t, plain, `kind "slack": 2 artifact(s) are not reproduced byte for byte`)
	if plain.SlackRechecked != 0 {
		t.Errorf("SlackRechecked = %d without R11", plain.SlackRechecked)
	}
	full := verifyAll(t, s.Case)
	for _, n := range full.Notices {
		if strings.Contains(n, "did not compose check R11") || strings.Contains(n, `kind "slack"`) {
			t.Errorf("notice with the checks composed: %s", n)
		}
	}
}

func TestVerifyChecksListsSlack(t *testing.T) {
	checks := VerifyChecks(Options{})
	if len(checks) != 4 {
		t.Fatalf("VerifyChecks has %d checks, want 4 (R6, R9, R10, R11)", len(checks))
	}
	e := newForgeEnv(t)
	l := e.fileLines()
	l[0].ImageOffset--
	e.add(t, l, nil)
	for i, ck := range checks {
		rep, err := e.c.VerifyWith(context.Background(), ck)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(slackProblems(rep)) > 0; got != (i == 3) {
			t.Errorf("check %d: slack problems %v, want them from the fourth check only", i, got)
		}
	}
}

func TestVerifySlackCancelKeepsFindings(t *testing.T) {
	e := newForgeEnv(t)
	l := e.fileLines()
	l[0].ImageOffset--
	file, _ := e.add(t, l, nil)
	e.addVol(t, []evidence.SlackMapLine{volLineOf(e.vol[0])})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := slackArtifactHook
	slackArtifactHook = func(id string) {
		if id != file.ID {
			cancel() // the second artifact starts after the first one's finding
		}
	}
	t.Cleanup(func() { slackArtifactHook = old })
	rep := verifySlackOnly(ctx, t, e.c)
	requireSlackProblem(t, rep, "is not the tail slack of the file")
}

func TestVerifySlackNarrowedArtifactIsClean(t *testing.T) {
	t.Run("a copy that failed between segments", func(t *testing.T) {
		f, _ := threeTails(t)
		c := newSlackCase(t)
		s := slackSessionOn(t, c, f.img, Options{})
		boom := errors.New("injected read failure")
		old := slackCopyHook
		slackCopyHook = func(i int) error {
			if i == 1 {
				return boom
			}
			return nil
		}
		t.Cleanup(func() { slackCopyHook = old })
		if _, err := exportSlack(t, s, SlackOptions{Partition: -1, Files: true}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		rep := verifyAll(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.SlackRechecked != 1 {
			t.Errorf("SlackRechecked = %d, want 1", rep.SlackRechecked)
		}
	})
	t.Run("a copy that failed inside a segment", func(t *testing.T) {
		f, _ := threeTails(t)
		c := newSlackCase(t)
		s := slackSessionOn(t, c, f.img, Options{})
		old := slackCutAfter
		slackCutAfter = func(i int) int64 {
			if i == 1 {
				return 3
			}
			return -1
		}
		t.Cleanup(func() { slackCutAfter = old })
		sum, err := exportSlack(t, s, SlackOptions{Partition: -1, Files: true})
		if err == nil {
			t.Fatal("the injected failure was not reported")
		}
		bin := slackRecord(t, sum, "p0-fat16/slack.bin")
		if !bin.Incomplete || bin.Source.Derived.Recovery.DeclaredRunsArtifact == "" {
			t.Fatalf("setup: bin %+v", bin)
		}
		rep := verifyAll(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.SlackRechecked != 1 {
			t.Errorf("SlackRechecked = %d, want 1", rep.SlackRechecked)
		}
	})
	t.Run("a cut directory", func(t *testing.T) {
		f, _ := fatCutSlack(t, true)
		s := slackSession(t, slices.Concat(f.img, pat(4096, 5)), Options{})
		sum := mustExportSlack(t, s, SlackOptions{Partition: -1})
		if !sum.Incomplete {
			t.Fatal("setup: the export is complete")
		}
		rep := verifyAll(t, s.Case)
		evidencetest.RequireClean(t, rep)
		if rep.SlackRechecked != 2 {
			t.Errorf("SlackRechecked = %d, want the incomplete file and the complete volume artifact", rep.SlackRechecked)
		}
	})
	t.Run("a zero-byte incomplete artifact with omitted lines", func(t *testing.T) {
		z, _ := fatCutSlack(t, false)
		s := slackSession(t, z.img, Options{})
		sum := mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
		bin := slackRecord(t, sum, "p0-fat16/slack.bin")
		if !bin.Incomplete || bin.Size != 0 {
			t.Fatalf("setup: bin %+v", bin)
		}
		rep := verifyAll(t, s.Case)
		evidencetest.RequireClean(t, rep)
		if rep.SlackRechecked != 1 {
			t.Errorf("SlackRechecked = %d, want 1", rep.SlackRechecked)
		}
	})
	t.Run("an omitted range that is not uniform is a problem", func(t *testing.T) {
		e := newForgeEnv(t)
		g := e.file[0]
		om := evidence.SlackMapLine{ImageOffset: g.ImageOffset, Length: g.Length, FSPath: g.FSPath, FSID: g.FSID, FileSize: g.FileSize, Uniform: true}
		e.add(t, []evidence.SlackMapLine{om}, nil)
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "is not uniform in the image")
	})
	t.Run("a narrowed line longer than its declared line is a problem", func(t *testing.T) {
		e := newForgeEnv(t)
		full := lineOf(e.file[0])
		short := full
		short.Length--
		// the declared map holds the line one byte short: the captured line is longer than the line it was cut from
		_, declared := e.add(t, []evidence.SlackMapLine{short}, func(s *evidencetest.SlackSpec) { s.Partition0, s.Partition = false, 1 })
		e.add(t, []evidence.SlackMapLine{full}, func(s *evidencetest.SlackSpec) {
			s.Mutate = func(r *evidence.Recovery) { r.DeclaredRunsArtifact = declared.ID }
		})
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "is longer (")
		// the control: a captured line shorter than its declared line is a prefix and passes
		e2 := newForgeEnv(t)
		_, decl2 := e2.add(t, []evidence.SlackMapLine{lineOf(e2.file[0])}, func(s *evidencetest.SlackSpec) { s.Partition0, s.Partition = false, 1 })
		cut := lineOf(e2.file[0])
		cut.Length--
		_, side := e2.add(t, []evidence.SlackMapLine{cut}, func(s *evidencetest.SlackSpec) {
			s.Mutate = func(r *evidence.Recovery) { r.DeclaredRunsArtifact = decl2.ID }
		})
		_ = side
		rep := verifySlackOnly(context.Background(), t, e2.c)
		// the declared artifact itself is a full incomplete artifact; only the narrowed one is under test
		for _, p := range slackProblems(rep) {
			if strings.Contains(p, "is longer (") || strings.Contains(p, "cut from") {
				t.Errorf("a strict prefix of its declared line is refused: %s", p)
			}
		}
	})
	t.Run("a shortened last line without a declared line is a problem", func(t *testing.T) {
		e := newForgeEnv(t)
		l := e.fileLines()
		l[len(l)-1].Length--
		e.add(t, l, nil)
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "is not the tail slack of the file")
	})
}

func TestVerifySlackRecheckDeletedDirentID(t *testing.T) {
	cs := int64(slackTestCS)
	f := newSlackFAT([]fattest.File{
		{Path: "A.TXT", Data: slackFill(5)},
		{Path: "GONE.TXT", Data: slackFill(cs - 5), Deleted: true},
	}, 2)
	f.patchTail("A.TXT", false, 5)
	off, n := f.patchTail("GONE.TXT", true, cs-5)
	c := newSlackCase(t)
	s := slackSessionOn(t, c, f.img, Options{})
	g := f.built.Find("GONE.TXT", true)
	deletedID := fmt.Sprintf("dirent:%d:%d", g.DirCluster, g.Index)
	evidencetest.AddSlack(t, c, s.Parent, f.img, evidencetest.SlackSpec{
		Partition0: true, FSType: "fat16", Incomplete: true, Params: map[string]string{"include_zero": "false"},
		Lines: []evidence.SlackMapLine{{ImageOffset: off, Length: n, FSPath: "/GONE.TXT", FSID: deletedID, FileSize: cs - 5, Exported: true}},
	})
	requireSlackProblem(t, verifySlackOnly(context.Background(), t, c), "entry is deleted")
}

func TestVerifySlackReplanMismatch(t *testing.T) {
	t.Run("the genuine export is clean", func(t *testing.T) {
		e := newForgeEnv(t)
		mustExportSlack(t, e.s, SlackOptions{Partition: -1, Files: true})
		rep := verifySlackOnly(context.Background(), t, e.c)
		requireNoSlackProblem(t, rep)
		if rep.SlackRechecked != 1 {
			t.Errorf("SlackRechecked = %d, want 1", rep.SlackRechecked)
		}
	})
	// reference exports the same image in a case of its own: the lines and counters a genuine run records
	reference := func(t *testing.T, e *forgeEnv) ([]evidence.SlackMapLine, map[string]string) {
		t.Helper()
		ref := slackSession(t, e.img, Options{})
		sum := mustExportSlack(t, ref, SlackOptions{Partition: -1, Files: true})
		bin := slackRecord(t, sum, "p0-fat16/slack.bin")
		lines := slackLinesOf(t, ref.Case, slackRecord(t, sum, "p0-fat16/slack.runs.jsonl"), evidence.MethodSlackFile)
		params := map[string]string{}
		for k, v := range bin.Source.Derived.Recovery.Params {
			params[k] = v
		}
		return lines, params
	}
	t.Run("one segment short", func(t *testing.T) {
		e := newForgeEnv(t)
		lines, params := reference(t, e)
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: lines[:len(lines)-1], Params: params})
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), fmt.Sprintf("the artifact lists %d exported segment(s), the re-plan has %d", len(lines)-1, len(lines)))
	})
	t.Run("a counter that differs", func(t *testing.T) {
		e := newForgeEnv(t)
		lines, params := reference(t, e)
		params["uniform_segments"] = "99"
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: lines, Params: params})
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), `parameter "uniform_segments" is "99", the re-plan gives`)
	})
	t.Run("the reference lines are clean as a complete artifact", func(t *testing.T) {
		e := newForgeEnv(t)
		lines, params := reference(t, e)
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: lines, Params: params})
		requireNoSlackProblem(t, verifySlackOnly(context.Background(), t, e.c))
	})
}

// A forged sidecar that repeats one omitted range over the whole image must not make verify scan the image
// once per line: omitted ranges and exported segments must be disjoint, and a violation is a problem that
// also stops the scan.
func TestVerifySlackOmittedRangesAreDisjoint(t *testing.T) {
	t.Run("one large range repeated", func(t *testing.T) {
		e := newForgeEnv(t)
		var lines []evidence.SlackMapLine
		for range 2000 {
			lines = append(lines, evidence.SlackMapLine{ImageOffset: 0, Length: int64(len(e.img)), FSPath: "/A.TXT", FSID: e.fsID["A.TXT"], FileSize: 2, Uniform: true})
		}
		e.add(t, lines, nil)
		rep := verifySlackOnly(context.Background(), t, e.c)
		requireSlackProblem(t, rep, "omitted range 0 and omitted range 1 overlap in the image")
		for _, p := range slackProblems(rep) {
			if strings.Contains(p, "not uniform") {
				t.Errorf("the overlapping ranges were scanned: %s", p)
			}
		}
	})
	t.Run("an omitted range over an exported segment", func(t *testing.T) {
		e := newForgeEnv(t)
		l := lineOf(e.file[0])
		om := l
		om.Exported, om.Uniform = false, true
		e.add(t, []evidence.SlackMapLine{l, om}, nil)
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "overlap in the image (omitted ranges are disjoint")
	})
	t.Run("disjoint ranges still pass", func(t *testing.T) {
		z, _ := fatCutSlack(t, false)
		s := slackSession(t, z.img, Options{})
		mustExportSlack(t, s, SlackOptions{Partition: -1, Files: true})
		requireNoSlackProblem(t, verifySlackOnly(context.Background(), t, s.Case))
	})
}

// A recorded path must name, now, the regular file its fs_id names.
func TestVerifySlackPathMustNameTheFile(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"another file", "/B.TXT", `now names`},
		{"a directory", "/", `names a dir, not a regular file`},
		{"a missing path", "/NOPE.TXT", `cannot be resolved`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newForgeEnv(t)
			l := e.fileLines()
			l[0].FSPath = tc.path
			e.add(t, l, nil)
			requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), tc.want)
		})
	}
}

// refExport exports one part of the image in a case of its own and returns its lines and counters, as a
// genuine run records them.
func refExport(t *testing.T, e *forgeEnv, o SlackOptions, suffix, method string) ([]evidence.SlackMapLine, map[string]string) {
	t.Helper()
	ref := slackSession(t, e.img, Options{})
	sum := mustExportSlack(t, ref, o)
	bin := slackRecord(t, sum, suffix+".bin")
	lines := slackLinesOf(t, ref.Case, slackRecord(t, sum, suffix+".runs.jsonl"), method)
	params := map[string]string{}
	for k, v := range bin.Source.Derived.Recovery.Params {
		params[k] = v
	}
	return lines, params
}

func TestVerifySlackCompleteForgedOffsetLengthAndVolume(t *testing.T) {
	file := func(t *testing.T) (*forgeEnv, []evidence.SlackMapLine, map[string]string) {
		e := newForgeEnv(t)
		l, p := refExport(t, e, SlackOptions{Partition: -1, Files: true}, "p0-fat16/slack", evidence.MethodSlackFile)
		return e, l, p
	}
	t.Run("one forged offset", func(t *testing.T) {
		e, l, p := file(t)
		l[0].ImageOffset--
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: l, Params: p})
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "segment 0 differs from the re-plan: image_offset is")
	})
	t.Run("one forged length", func(t *testing.T) {
		e, l, p := file(t)
		l[1].Length--
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: l, Params: p})
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e.c), "segment 1 differs from the re-plan: length is")
	})
	t.Run("a volume artifact", func(t *testing.T) {
		e := newForgeEnv(t)
		l, p := refExport(t, e, SlackOptions{Partition: -1, Volume: true}, "p0-fat16/volume-slack", evidence.MethodSlackVolume)
		spec := evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Method: evidence.MethodSlackVolume, Lines: l, Params: p}
		evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, spec)
		requireNoSlackProblem(t, verifySlackOnly(context.Background(), t, e.c))
		e2 := newForgeEnv(t)
		l[0].Length--
		spec.Lines = l
		evidencetest.AddSlack(t, e2.c, e2.s.Parent, e2.img, spec)
		requireSlackProblem(t, verifySlackOnly(context.Background(), t, e2.c), "segment 0 differs from the re-plan: length is")
	})
}

// One group holding artifacts of two include_zero settings: each is compared with a re-plan of its own setting.
func TestVerifySlackTwoIncludeZeroSettingsInOneGroup(t *testing.T) {
	e := newForgeEnv(t)
	fl, fp := refExport(t, e, SlackOptions{Partition: -1, Files: true, IncludeZero: true}, "p0-fat16/slack", evidence.MethodSlackFile)
	vl, vp := refExport(t, e, SlackOptions{Partition: -1, Volume: true}, "p0-fat16/volume-slack", evidence.MethodSlackVolume)
	if fp["include_zero"] != "true" || vp["include_zero"] != "false" {
		t.Fatalf("setup: include_zero %q and %q", fp["include_zero"], vp["include_zero"])
	}
	evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Lines: fl, Params: fp})
	evidencetest.AddSlack(t, e.c, e.s.Parent, e.img, evidencetest.SlackSpec{Partition0: true, FSType: "fat16", Method: evidence.MethodSlackVolume, Lines: vl, Params: vp})
	rep := verifySlackOnly(context.Background(), t, e.c)
	requireNoSlackProblem(t, rep)
	if rep.SlackRechecked != 2 {
		t.Errorf("SlackRechecked = %d, want 2", rep.SlackRechecked)
	}
}
