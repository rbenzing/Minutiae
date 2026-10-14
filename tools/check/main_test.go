package main

import (
	"slices"
	"testing"
)

// The FAST check uses Go's test cache; the RACE merge gate never does.
func TestFastTestStepsUseTheTestCache(t *testing.T) {
	steps := testStepsFor(false)
	if len(steps) == 0 {
		t.Fatal("no fast test steps")
	}
	for _, s := range steps {
		if slices.Contains(s.args, "-count=1") {
			t.Errorf("a fast test step disables the test cache: %v", s.args)
		}
		if len(s.args) < 2 || s.args[1] != "test" {
			t.Errorf("fast step is not a go test run: %v", s.args)
		}
	}
}

func TestRaceTestStepsAreUncached(t *testing.T) {
	t.Chdir("../..") // the prepare functions list packages relative to the module root
	steps := testStepsFor(true)
	if len(steps) != 3 {
		t.Fatalf("race gate has %d steps, want 3", len(steps))
	}
	for i, s := range steps {
		args := s.args
		if s.prepare != nil {
			var err error
			if args, err = s.prepare(); err != nil {
				t.Fatalf("step %d prepare: %v", i+1, err)
			}
		}
		if !slices.Contains(args, "-count=1") {
			t.Errorf("race-gate step %d runs tests with the cache enabled: %v", i+1, args)
		}
	}
}
