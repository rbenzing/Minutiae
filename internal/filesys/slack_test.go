package filesys_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

func TestUnitRuns(t *testing.T) {
	r := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	tests := []struct {
		name string
		in   []filesys.Run
		unit int64
		want []filesys.Run
	}{
		{"round up", []filesys.Run{r(1024, 700)}, 512, []filesys.Run{r(1024, 1024)}},
		{"holes dropped", []filesys.Run{r(0, 512), r(-1, 512), r(2048, 1)}, 512, []filesys.Run{r(0, 512), r(2048, 512)}},
		{"nil input", nil, 512, nil},
		{"unit zero", []filesys.Run{r(0, 512)}, 0, nil},
		{"unit negative", []filesys.Run{r(0, 512)}, -1, nil},
		{"offset overflow", []filesys.Run{r(math.MaxInt64-10, 5)}, 512, nil},
		{"length overflow", []filesys.Run{r(0, math.MaxInt64-3)}, 512, nil},
		{"only holes", []filesys.Run{r(-1, 700)}, 512, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filesys.UnitRuns(tc.in, tc.unit); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UnitRuns(%v, %d) = %v, want %v", tc.in, tc.unit, got, tc.want)
			}
		})
	}
	in := []filesys.Run{{Offset: 0, Length: 10}}
	got := filesys.UnitRuns(in, 512)
	if len(got) != 1 {
		t.Fatalf("UnitRuns(%v, 512) = %v, want one run", in, got)
	}
	got[0].Length = 1
	if again := filesys.UnitRuns(in, 512); len(again) != 1 || again[0].Length != 512 || in[0].Length != 10 {
		t.Errorf("UnitRuns result aliases state: %v, input %v", again, in)
	}
}

func TestCheckAllocatedRuns(t *testing.T) {
	r := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	const fsSize = 1 << 20
	tests := []struct {
		name        string
		runs, alloc []filesys.Run
		unit        int64
		bad         bool
	}{
		{"valid", []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 1024)}, 512, false},
		{"valid with hole", []filesys.Run{r(0, 512), r(-1, 512), r(2048, 1)}, []filesys.Run{r(0, 512), r(2048, 512)}, 512, false},
		{"no data runs", []filesys.Run{r(-1, 512)}, nil, 512, false},
		{"unit zero", []filesys.Run{r(0, 10)}, []filesys.Run{r(0, 10)}, 0, true},
		{"unit negative", nil, nil, -512, true},
		{"count short", []filesys.Run{r(0, 10), r(4096, 10)}, []filesys.Run{r(0, 512)}, 512, true},
		{"count long", []filesys.Run{r(0, 10)}, []filesys.Run{r(0, 512), r(4096, 512)}, 512, true},
		{"offset differs", []filesys.Run{r(512, 10)}, []filesys.Run{r(0, 512)}, 512, true},
		{"one unit short", []filesys.Run{r(0, 700)}, []filesys.Run{r(0, 512)}, 512, true},
		{"one unit long", []filesys.Run{r(0, 700)}, []filesys.Run{r(0, 1536)}, 512, true},
		{"past fs", []filesys.Run{r(fsSize-100, 100)}, []filesys.Run{r(fsSize-100, 512)}, 512, true},
		{"overlap", []filesys.Run{r(0, 700), r(1000, 10)}, []filesys.Run{r(0, 1024), r(1000, 512)}, 512, true},
		{"overlap out of order", []filesys.Run{r(1000, 10), r(0, 700)}, []filesys.Run{r(1000, 512), r(0, 1024)}, 512, true},
		{"overflow", []filesys.Run{r(math.MaxInt64-10, 5)}, []filesys.Run{r(math.MaxInt64-10, 512)}, 512, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs, alloc := append([]filesys.Run(nil), tc.runs...), append([]filesys.Run(nil), tc.alloc...)
			err := filesys.CheckAllocatedRuns(tc.runs, tc.alloc, tc.unit, fsSize)
			if tc.bad {
				if !errors.Is(err, filesys.ErrCorrupt) {
					t.Fatalf("err = %v, want ErrCorrupt", err)
				}
				var ce *filesys.CorruptError
				if !errors.As(err, &ce) || ce.Structure != "allocation units" {
					t.Errorf("err = %v, want a *CorruptError with Structure \"allocation units\"", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !reflect.DeepEqual(runs, append([]filesys.Run(nil), tc.runs...)) || !reflect.DeepEqual(alloc, append([]filesys.Run(nil), tc.alloc...)) {
				t.Errorf("CheckAllocatedRuns modified its input")
			}
		})
	}
	// The overlap test sorts a copy: the caller's slice keeps its order.
	alloc := []filesys.Run{r(1000, 512), r(0, 1024)}
	_ = filesys.CheckAllocatedRuns([]filesys.Run{r(1000, 10), r(0, 700)}, alloc, 512, fsSize)
	if alloc[0].Offset != 1000 {
		t.Errorf("the alloc slice was sorted in place: %v", alloc)
	}
}

func TestTailSlack(t *testing.T) {
	r := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	const unit, fsSize = 512, 1 << 20
	tests := []struct {
		name        string
		size        int64
		runs, alloc []filesys.Run
		wantRun     filesys.Run
		want        filesys.SlackOutcome
		reason      bool
	}{
		{"slack", 700, []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 1024)}, r(4796, 324), filesys.SlackFound, false},
		{"exact fit", 1024, []filesys.Run{r(4096, 1024)}, []filesys.Run{r(4096, 1024)}, filesys.Run{}, filesys.SlackExactFit, false},
		{"slack after hole", 1500, []filesys.Run{r(-1, 512), r(4096, 988)}, []filesys.Run{r(4096, 1024)}, r(5084, 36), filesys.SlackFound, false},
		{"size zero", 0, nil, nil, filesys.Run{}, filesys.SlackNoRuns, false},
		{"nil runs", 700, nil, nil, filesys.Run{}, filesys.SlackNoRuns, false},
		{"all holes", 700, []filesys.Run{r(-1, 700)}, nil, filesys.Run{}, filesys.SlackNoRuns, false},
		{"prefix", 700, []filesys.Run{r(4096, 512)}, []filesys.Run{r(4096, 512)}, filesys.Run{}, filesys.SlackPrefix, false},
		{"trailing hole", 700, []filesys.Run{r(4096, 512), r(-1, 188)}, []filesys.Run{r(4096, 512)}, filesys.Run{}, filesys.SlackTrailingHole, false},
		{"alloc missing", 700, []filesys.Run{r(4096, 700)}, nil, filesys.Run{}, filesys.SlackAllocMismatch, true},
		{"alloc short", 700, []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 512)}, filesys.Run{}, filesys.SlackAllocMismatch, true},
		{"past fs", 700, []filesys.Run{r(fsSize-100, 700)}, []filesys.Run{r(fsSize-100, 1024)}, filesys.Run{}, filesys.SlackInvalidRuns, true},
		{"covers more than size", 100, []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 1024)}, filesys.Run{}, filesys.SlackInvalidRuns, true},
		{"negative size", -1, []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 1024)}, filesys.Run{}, filesys.SlackInvalidRuns, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, out, reason := filesys.TailSlack(tc.size, tc.runs, tc.alloc, unit, fsSize)
			if out != tc.want || run != tc.wantRun {
				t.Errorf("TailSlack = %v, %q; want %v, %q", run, out, tc.wantRun, tc.want)
			}
			if tc.reason && reason == "" {
				t.Errorf("outcome %q came without a reason", out)
			}
			if !tc.reason && reason != "" {
				t.Errorf("outcome %q came with the reason %q", out, reason)
			}
		})
	}
}

type slackFS struct {
	filesys.FileSystem
	v bool
}

func (s *slackFS) ReportsSlack() bool { return s.v }

type slackWrap struct {
	filesys.FileSystem
	inner filesys.FileSystem
}

func (w slackWrap) Underlying() filesys.FileSystem { return w.inner }

func TestReportsSlack(t *testing.T) {
	yes, no := &slackFS{v: true}, &slackFS{v: false}
	var typedNil *slackFS
	if !filesys.ReportsSlack(yes) {
		t.Errorf("a reporter returning true is not reported")
	}
	if filesys.ReportsSlack(no) {
		t.Errorf("a reporter returning false is reported")
	}
	if !filesys.ReportsSlack(slackWrap{inner: yes}) {
		t.Errorf("a wrapper over a reporter is not reported")
	}
	if filesys.ReportsSlack(slackWrap{inner: no}) {
		t.Errorf("a wrapper over a false reporter is reported")
	}
	if filesys.ReportsSlack(typedNil) {
		t.Errorf("a typed nil reporter is reported")
	}
	if filesys.ReportsSlack(nil) {
		t.Errorf("nil is reported")
	}
	if filesys.ReportsSlack(baseFS{}) {
		t.Errorf("a filesystem without the method is reported")
	}
	if !errors.Is(filesys.ErrNoSlack, filesys.ErrUnsupported) {
		t.Errorf("ErrNoSlack does not wrap ErrUnsupported")
	}
}
