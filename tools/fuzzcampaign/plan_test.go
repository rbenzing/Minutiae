package main

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var sampleTargets = []Target{
	{"internal/decode/plist", "FuzzPlist"},
	{"internal/decode/sqlitedb", "FuzzSQLiteDB"},
	{"internal/decodex", "FuzzNotDecode"},
	{"internal/dbrecover", "FuzzRun"},
	{"internal/filesys/fat", "FuzzFATOpen"},
	{"internal/filesys", "FuzzWalk"},
	{"internal/parsers/androidmms", "FuzzMMS"},
	{"internal/recordtypes/call", "FuzzCall"},
	{"internal/records", "FuzzResolveChain"},
	{"internal/sqlitefile", "FuzzOpen"},
}

func names(ts []Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Pkg+"."+t.Name)
	}
	return out
}

func itemTargets(p Plan) []Target {
	var out []Target
	for _, it := range p.Items {
		out = append(out, it.Target)
	}
	return out
}

func TestBuildPlanProfiles(t *testing.T) {
	want := map[string][]string{
		"decoders": {"internal/decode/plist.FuzzPlist", "internal/decode/sqlitedb.FuzzSQLiteDB"},
		"readers": {
			"internal/dbrecover.FuzzRun", "internal/filesys.FuzzWalk",
			"internal/filesys/fat.FuzzFATOpen", "internal/sqlitefile.FuzzOpen",
		},
		"parsers": {"internal/parsers/androidmms.FuzzMMS", "internal/recordtypes/call.FuzzCall"},
	}
	for name, w := range want {
		p, err := BuildPlan(sampleTargets, DefaultOptions(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := names(itemTargets(p)); !slices.Equal(got, w) {
			t.Errorf("%s selects %v, want %v", name, got, w)
		}
	}
	p, err := BuildPlan(sampleTargets, DefaultOptions("all"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != len(sampleTargets) {
		t.Errorf("all selects %d targets, want %d", len(p.Items), len(sampleTargets))
	}
}

func TestBuildPlanPrefixIsAPathPrefixNotAStringPrefix(t *testing.T) {
	got, err := List(sampleTargets, DefaultOptions("decoders"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(got), "internal/decodex.FuzzNotDecode") {
		t.Errorf("internal/decodex matched the prefix internal/decode: %v", names(got))
	}
	o := DefaultOptions("all")
	o.Pkgs = []string{"internal/filesys"}
	got, err = List(sampleTargets, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/filesys.FuzzWalk", "internal/filesys/fat.FuzzFATOpen"}; !slices.Equal(names(got), want) {
		t.Errorf("Pkgs prefix internal/filesys selects %v, want %v", names(got), want)
	}
}

func TestBuildPlanPkgsNarrowTheProfile(t *testing.T) {
	o := DefaultOptions("readers")
	o.Pkgs = []string{"internal/dbrecover", "internal/sqlitefile"}
	p, err := BuildPlan(sampleTargets, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/dbrecover.FuzzRun", "internal/sqlitefile.FuzzOpen"}; !slices.Equal(names(itemTargets(p)), want) {
		t.Errorf("selected %v, want %v", names(itemTargets(p)), want)
	}
	o.Profile = "parsers"
	if _, err := BuildPlan(sampleTargets, o); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("profile parsers with reader Pkgs: err = %v, want ErrEmptySelection", err)
	}
}

func TestBuildPlanEmptySelectionIsAnError(t *testing.T) {
	if _, err := BuildPlan(nil, DefaultOptions("all")); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("BuildPlan(nil): err = %v, want ErrEmptySelection", err)
	}
	if _, err := List(nil, DefaultOptions("all")); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("List(nil): err = %v, want ErrEmptySelection", err)
	}
	o := DefaultOptions("all")
	o.Pkgs = []string{"internal/nothing"}
	if _, err := List(sampleTargets, o); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("List with an unmatched Pkgs: err = %v, want ErrEmptySelection", err)
	}
}

func TestDefaultOptions(t *testing.T) {
	got := DefaultOptions("decoders")
	want := PlanOptions{Profile: "decoders", PerTarget: 120 * time.Second, Minimize: 10 * time.Second, Total: 90 * time.Minute, Parallel: 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultOptions = %+v, want %+v", got, want)
	}
}

func TestBuildPlanEstimate(t *testing.T) {
	ts := []Target{{"a", "FuzzA"}, {"a", "FuzzB"}, {"b", "FuzzC"}}
	p, err := BuildPlan(ts, DefaultOptions("all"))
	if err != nil {
		t.Fatal(err)
	}
	// three items of 120 s + 10 s, plus 3 minutes for each of the two packages.
	if want := 3*130*time.Second + 2*3*time.Minute; p.Estimate != want {
		t.Errorf("Estimate = %v, want %v", p.Estimate, want)
	}
	for _, it := range p.Items {
		if it.Fuzztime != 120*time.Second || it.Minimize != 10*time.Second {
			t.Errorf("item %v: Fuzztime %v Minimize %v", it.Target, it.Fuzztime, it.Minimize)
		}
	}
	if !reflect.DeepEqual(p.Options, DefaultOptions("all")) {
		t.Errorf("Options not recorded: %+v", p.Options)
	}
}

func TestBuildPlanRefusesOutOfRangeOptions(t *testing.T) {
	ts := []Target{{"a", "FuzzA"}}
	cases := []struct {
		name  string
		field string
		set   func(*PlanOptions)
		ok    bool
	}{
		{"profile unknown", "Profile", func(o *PlanOptions) { o.Profile = "nope" }, false},
		{"profile empty", "Profile", func(o *PlanOptions) { o.Profile = "" }, false},
		{"pkgs empty entry", "Pkgs", func(o *PlanOptions) { o.Pkgs = []string{""} }, false},
		{"per-target min", "PerTarget", func(o *PlanOptions) { o.PerTarget = 10 * time.Second }, true},
		{"per-target below min", "PerTarget", func(o *PlanOptions) { o.PerTarget = 10*time.Second - 1 }, false},
		{"per-target zero", "PerTarget", func(o *PlanOptions) { o.PerTarget = 0 }, false},
		{"per-target max", "PerTarget", func(o *PlanOptions) { o.PerTarget = 15 * time.Minute }, true},
		{"per-target above max", "PerTarget", func(o *PlanOptions) { o.PerTarget = 15*time.Minute + 1 }, false},
		{"minimize zero", "Minimize", func(o *PlanOptions) { o.Minimize = 0 }, true},
		{"minimize negative", "Minimize", func(o *PlanOptions) { o.Minimize = -1 }, false},
		{"minimize max", "Minimize", func(o *PlanOptions) { o.Minimize = 60 * time.Second }, true},
		{"minimize above max", "Minimize", func(o *PlanOptions) { o.Minimize = 60*time.Second + 1 }, false},
		{"parallel min", "Parallel", func(o *PlanOptions) { o.Parallel = 1 }, true},
		{"parallel zero", "Parallel", func(o *PlanOptions) { o.Parallel = 0 }, false},
		{"parallel max", "Parallel", func(o *PlanOptions) { o.Parallel = 4 }, true},
		{"parallel above max", "Parallel", func(o *PlanOptions) { o.Parallel = 5 }, false},
		{"total min", "Total", func(o *PlanOptions) { o.Total = 10 * time.Minute }, true},
		{"total below min", "Total", func(o *PlanOptions) { o.Total = 10*time.Minute - 1 }, false},
		{"total max", "Total", func(o *PlanOptions) { o.Total = 240 * time.Minute }, true},
		{"total above max", "Total", func(o *PlanOptions) { o.Total = 240*time.Minute + 1 }, false},
	}
	for _, c := range cases {
		o := DefaultOptions("all")
		c.set(&o)
		_, err := BuildPlan(ts, o)
		if c.ok {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var oe *OptionError
		if !errors.As(err, &oe) || oe.Field != c.field {
			t.Errorf("%s: err = %v, want an *OptionError for %s", c.name, err, c.field)
		}
		// List validates the selection inputs only (the profile and the package filters).
		_, lerr := List(ts, o)
		if selection := c.field == "Profile" || c.field == "Pkgs"; selection && lerr == nil {
			t.Errorf("%s: List accepted it", c.name)
		} else if !selection && lerr != nil {
			t.Errorf("%s: List refused an option it does not use: %v", c.name, lerr)
		}
	}
}

func TestTotalCapRefusesAPlanThatDoesNotFit(t *testing.T) {
	var ts []Target
	for i := range 13 {
		ts = append(ts, Target{"internal/p", "Fuzz" + string(rune('A'+i))})
	}
	o := DefaultOptions("all")
	o.PerTarget = 15 * time.Minute
	o.Total = 90 * time.Minute
	p, err := BuildPlan(ts, o)
	if !errors.Is(err, ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	estimate := 13*(15*time.Minute+10*time.Second) + 3*time.Minute
	for _, want := range []string{estimate.String(), (90 * time.Minute).String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if !reflect.DeepEqual(p, Plan{}) {
		t.Errorf("the refused plan is not the zero Plan: %+v", p)
	}
	// The same selection is listed without a cap check.
	got, err := List(ts, o)
	if err != nil || len(got) != 13 {
		t.Errorf("List = %d targets, %v; want 13, nil", len(got), err)
	}
}

func TestListHasNoCapCheck(t *testing.T) {
	var ts []Target
	for i := range 13 {
		ts = append(ts, Target{"internal/p" + string(rune('a'+i)), "FuzzX"})
	}
	o := DefaultOptions("all")
	o.PerTarget = 15 * time.Minute
	got, err := List(ts, o)
	if err != nil || !slices.Equal(got, ts) {
		t.Errorf("List = %v, %v", got, err)
	}
}

func TestBuildPlanIsDeterministic(t *testing.T) {
	rev := slices.Clone(sampleTargets)
	slices.Reverse(rev)
	a, err := BuildPlan(sampleTargets, DefaultOptions("all"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildPlan(rev, DefaultOptions("all"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the plan depends on the input order:\n%+v\n%+v", a, b)
	}
	if !slices.IsSortedFunc(itemTargets(a), cmpTarget) {
		t.Errorf("the items are not sorted: %v", itemTargets(a))
	}
}

func TestBuildPlanMinimizeOverride(t *testing.T) {
	old := overrides
	overrides = map[string]time.Duration{"internal/decode/plist.FuzzPlist": 0, "internal/records.FuzzResolveChain": 30 * time.Second}
	t.Cleanup(func() { overrides = old })
	p, err := BuildPlan(sampleTargets, DefaultOptions("all"))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		want := 10 * time.Second
		switch it.Pkg + "." + it.Name {
		case "internal/decode/plist.FuzzPlist":
			want = 0
		case "internal/records.FuzzResolveChain":
			want = 30 * time.Second
		}
		if it.Minimize != want {
			t.Errorf("%v: Minimize = %v, want %v", it.Target, it.Minimize, want)
		}
	}
}

// TestShippedMinimizeOverrides pins the table Task 1 produced: FuzzSQLiteDB is run with
// minimisation off (docs/testing.md "Fuzz stall findings"), and nothing else is overridden.
func TestShippedMinimizeOverrides(t *testing.T) {
	want := map[string]time.Duration{"internal/decode/sqlitedb.FuzzSQLiteDB": 0}
	if !reflect.DeepEqual(overrides, want) {
		t.Errorf("overrides = %v, want %v", overrides, want)
	}
}

func TestTotalCapAcceptsAPlanThatFitsExactly(t *testing.T) {
	mk := func(n int) []Target {
		var ts []Target
		for i := range n {
			ts = append(ts, Target{"internal/p", "Fuzz" + string(rune('A'+i))})
		}
		return ts
	}
	o := DefaultOptions("all")
	o.PerTarget, o.Minimize, o.Total = time.Minute, 0, 10*time.Minute
	// seven targets of 1 minute and 3 minutes for the package are exactly 10 minutes.
	p, err := BuildPlan(mk(7), o)
	if err != nil || p.Estimate != 10*time.Minute {
		t.Fatalf("seven targets: estimate %v, err %v; want exactly 10m, nil", p.Estimate, err)
	}
	if _, err := BuildPlan(mk(8), o); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("eight targets: err = %v, want ErrOverBudget", err)
	}
	o.PerTarget = 7*time.Minute + 1
	if _, err := BuildPlan(mk(1), o); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("one target one nanosecond over: err = %v, want ErrOverBudget", err)
	}
}
