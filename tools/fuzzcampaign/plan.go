package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Profile names a set of package-path prefixes.
type Profile struct {
	Name     string
	Prefixes []string // a Target matches when its Pkg equals a prefix or starts with prefix+"/"
}

// Profiles are the selectable campaigns. The prefix "" matches every package.
var Profiles = []Profile{
	{"decoders", []string{"internal/decode"}},
	{"readers", []string{"internal/sqlitefile", "internal/dbrecover", "internal/filesys", "internal/image", "internal/volume"}},
	{"parsers", []string{"internal/parsers", "internal/recordtypes"}},
	{"all", []string{""}},
}

// Bounds on the plan options; a value outside them is an error, never clamped.
const (
	minPerTarget = 10 * time.Second
	maxPerTarget = 15 * time.Minute
	maxMinimize  = 60 * time.Second
	minTotal     = 10 * time.Minute
	maxTotal     = 240 * time.Minute
	maxParallel  = 4
	// perPackage is the allowance in the estimate for building each distinct package.
	perPackage = 3 * time.Minute
)

// PlanOptions are the campaign settings. BuildPlan applies none of them by default: use
// DefaultOptions for the defaults.
type PlanOptions struct {
	Profile   string
	Pkgs      []string      // narrows the profile: a Target must match the profile AND one of these prefixes
	PerTarget time.Duration // 10s..15m
	Minimize  time.Duration // 0..60s
	Total     time.Duration // cap on the whole campaign, 10m..240m
	Parallel  int           // 1..4
}

// DefaultOptions returns the defaults for a profile: 120 s per target, 10 s of minimisation,
// a 90 minute cap and two in parallel.
func DefaultOptions(profile string) PlanOptions {
	return PlanOptions{Profile: profile, PerTarget: 120 * time.Second, Minimize: 10 * time.Second, Total: 90 * time.Minute, Parallel: 2}
}

// Item is one target with its time allowance.
type Item struct {
	Target
	Fuzztime, Minimize time.Duration
}

// Plan is the ordered list of items and what it is expected to cost.
type Plan struct {
	Items    []Item
	Estimate time.Duration // sum of Fuzztime+Minimize per item + 3 minutes per distinct package
	Options  PlanOptions
}

var (
	ErrOverBudget     = errors.New("fuzzcampaign: the plan does not fit the total cap")
	ErrEmptySelection = errors.New("fuzzcampaign: the profile and package filters select no target")
)

// OptionError reports one option outside its range.
type OptionError struct{ Field, Value, Range string }

func (e *OptionError) Error() string {
	return fmt.Sprintf("fuzzcampaign: option %s = %s is outside %s", e.Field, e.Value, e.Range)
}

// overrides sets the minimisation time of single targets, keyed "<Pkg>.<Name>". FuzzSQLiteDB
// runs without minimisation: with it the engine's counter freezes for the rest of a run
// (docs/testing.md, "Fuzz stall findings").
var overrides = map[string]time.Duration{
	"internal/decode/sqlitedb.FuzzSQLiteDB": 0,
}

func matchesPrefix(pkg, prefix string) bool {
	return prefix == "" || pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

func matchesAny(pkg string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return matchesPrefix(pkg, p) })
}

// List returns the targets selected by the profile and the Pkgs filter, sorted, with no cap
// check. An empty selection is ErrEmptySelection.
func List(ts []Target, o PlanOptions) ([]Target, error) {
	i := slices.IndexFunc(Profiles, func(p Profile) bool { return p.Name == o.Profile })
	if i < 0 {
		return nil, &OptionError{"Profile", fmt.Sprintf("%q", o.Profile), "the profiles " + profileNames()}
	}
	for _, p := range o.Pkgs {
		if p == "" {
			return nil, &OptionError{"Pkgs", `""`, "non-empty package prefixes"}
		}
	}
	var out []Target
	for _, t := range ts {
		if matchesAny(t.Pkg, Profiles[i].Prefixes) && (len(o.Pkgs) == 0 || matchesAny(t.Pkg, o.Pkgs)) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, ErrEmptySelection
	}
	slices.SortFunc(out, cmpTarget)
	return slices.Compact(out), nil
}

func profileNames() string {
	var names []string
	for _, p := range Profiles {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

func checkOptions(o PlanOptions) error {
	switch {
	case o.PerTarget < minPerTarget || o.PerTarget > maxPerTarget:
		return &OptionError{"PerTarget", o.PerTarget.String(), minPerTarget.String() + " to " + maxPerTarget.String()}
	case o.Minimize < 0 || o.Minimize > maxMinimize:
		return &OptionError{"Minimize", o.Minimize.String(), "0s to " + maxMinimize.String()}
	case o.Parallel < 1 || o.Parallel > maxParallel:
		return &OptionError{"Parallel", fmt.Sprint(o.Parallel), fmt.Sprintf("1 to %d", maxParallel)}
	case o.Total < minTotal || o.Total > maxTotal:
		return &OptionError{"Total", o.Total.String(), minTotal.String() + " to " + maxTotal.String()}
	}
	return nil
}

// BuildPlan selects the targets and prices the plan. Options outside their ranges are an
// *OptionError, an empty selection is ErrEmptySelection, and a plan whose estimate exceeds
// Total is ErrOverBudget (naming both numbers) and the zero Plan: a plan is never shortened.
func BuildPlan(ts []Target, o PlanOptions) (Plan, error) {
	sel, err := List(ts, o)
	if err != nil {
		return Plan{}, err
	}
	if err := checkOptions(o); err != nil {
		return Plan{}, err
	}
	p := Plan{Options: o}
	pkgs := map[string]bool{}
	for _, t := range sel {
		it := Item{Target: t, Fuzztime: o.PerTarget, Minimize: o.Minimize}
		if m, ok := overrides[t.Pkg+"."+t.Name]; ok {
			it.Minimize = m
		}
		p.Items = append(p.Items, it)
		p.Estimate += it.Fuzztime + it.Minimize
		pkgs[t.Pkg] = true
	}
	p.Estimate += time.Duration(len(pkgs)) * perPackage
	if p.Estimate > o.Total {
		return Plan{}, fmt.Errorf("%w: the estimate is %s for %d targets, the cap is %s", ErrOverBudget, p.Estimate, len(p.Items), o.Total)
	}
	return p, nil
}
