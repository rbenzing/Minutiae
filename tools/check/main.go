// Command check runs every verification step that must pass before work is
// called done: module tidiness, vet, lint (incl. formatting), build, the
// CGO_ENABLED=0 host build and cross-builds, and tests.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type step struct {
	env  []string // extra environment, KEY=VALUE
	args []string
	// prepare, when set, builds args just before the step runs; an error fails the check.
	prepare func() ([]string, error)
}

func main() {
	steps := []step{
		{args: []string{"go", "mod", "tidy", "-diff"}},
		{args: []string{"go", "vet", "./..."}},
		{args: []string{"go", "run", "./tools/parserhash", "-check"}},
		{args: []string{"go", "tool", "golangci-lint", "run", "./..."}},
		{args: []string{"go", "build", "-o", binPath(), "./cmd/minutiae"}},
		// cgo policy: the pure-Go (CGO_ENABLED=0) static build must always work
		// on every supported OS, so it is proven on the host too (cgo defaults
		// to on wherever a C toolchain exists).
		pureBuild(),
		crossBuild("linux", "amd64"),
		crossBuild("darwin", "arm64"),
	}
	steps = append(steps, testSteps()...)
	for _, s := range steps {
		if s.prepare != nil {
			args, err := s.prepare()
			if err != nil {
				fmt.Fprintf(os.Stderr, "CHECK FAILED preparing a test step: %v\n", err)
				os.Exit(1)
			}
			s.args = args
		}
		line := strings.Join(append(append([]string{}, s.env...), s.args...), " ")
		fmt.Printf("==> %s\n", line)
		cmd := exec.Command(s.args[0], s.args[1:]...) //nolint:gosec // fixed, trusted command list
		cmd.Env = append(os.Environ(), s.env...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "CHECK FAILED at %q: %v\n", line, err)
			os.Exit(1)
		}
	}
	fmt.Println("CHECK PASSED")
}

// pureBuild compiles the CLI for the host with cgo disabled and discards the
// binary.
func pureBuild() step {
	return step{
		env:  []string{"CGO_ENABLED=0"},
		args: []string{"go", "build", "-o", os.DevNull, "./cmd/minutiae"},
	}
}

// crossBuild compiles the CLI for goos/goarch without cgo and discards the
// binary, so no stray build output is left in the repository.
func crossBuild(goos, goarch string) step {
	return step{
		env:  []string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch},
		args: []string{"go", "build", "-o", os.DevNull, "./cmd/minutiae"},
	}
}

func binPath() string {
	p := filepath.Join("bin", "minutiae")
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	return p
}

// raceConcurrencyTests names, by hand, the tests of internal/evidence and
// internal/records that start goroutines, use t.Parallel or exercise
// concurrent, queued or nested transactions, live-ingest exclusion, the case
// lock or concurrent Add. Those two packages are too slow as a whole under the
// race detector (modernc sqlite instrumented for -race exceeds the per-package
// timeout; no test hangs), so the race run covers only these tests, while the
// first step runs every test without -race. Every name must match a real test:
// the check fails otherwise, so the list cannot rot silently.
var raceConcurrencyTests = []string{
	// internal/evidence
	"TestConcurrentArtifacts",
	"TestCaseLockExcludesSecondOpen",
	"TestUpgradeRefusedWhenCaseInUse",
	"TestNormalizeDeterministicAndConcurrent",
	"TestFTSNormVersionShape",
	"TestNestedTxIsRefusedNotDeadlocked",
	"TestConcurrentTxQueueAndSucceed",
	"TestNestedTxFailsWhileOthersAreQueued",
	"TestQueuedTxHonoursContext",
	// internal/records
	"TestAddConcurrent",
	"TestAddConcurrentWithIndex",
	"TestLiveIngestBlocksSecondStart",
	"TestReindexRefusedWhileIngestActive",
	"TestVerifyLiveIngestIsNotInterrupted",
	"TestCanonicalPayloadRejectsHostile",
	"TestVerifyTerminatesOnBlobRunArtifactRows",
	"TestVerifyDetectsAnyColumnAlteration",
}

// slowRacePackages are tested under -race only through raceConcurrencyTests.
var slowRacePackages = []string{"./internal/evidence", "./internal/records"}

// testSteps returns the test steps. Without a race detector: one plain run.
// With it: (1) every test without -race, (2) every package except the slow
// ones under -race, (3) the concurrency tests of the slow packages under -race.
func testSteps() []step {
	if !raceSupported() {
		fmt.Println("note: race detector unavailable (needs cgo and a C compiler); running tests without -race")
		return []step{{args: []string{"go", "test", "-count=1", "./..."}}}
	}
	return []step{
		{args: []string{"go", "test", "-count=1", "./..."}},
		{prepare: raceOthersCmd},
		{prepare: raceConcurrencyCmd},
	}
}

func raceOthersCmd() ([]string, error) {
	out, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w", err)
	}
	mod, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	prefix := strings.TrimSpace(string(mod))
	skip := map[string]bool{}
	for _, p := range slowRacePackages {
		skip[prefix+strings.TrimPrefix(p, ".")] = true
	}
	args := []string{"go", "test", "-race", "-timeout", "30m", "-count=1"}
	n := 0
	for _, p := range strings.Fields(string(out)) {
		if !skip[p] {
			args = append(args, p)
			n++
		}
	}
	if n == 0 {
		return nil, errors.New("no packages left for the race run")
	}
	return args, nil
}

func raceConcurrencyCmd() ([]string, error) {
	listed := map[string]bool{}
	for _, p := range slowRacePackages {
		out, err := exec.Command("go", "test", "-list", ".", p).Output()
		if err != nil {
			return nil, fmt.Errorf("go test -list %s: %w", p, err)
		}
		for _, l := range strings.Fields(string(out)) {
			listed[l] = true
		}
	}
	var missing []string
	for _, n := range raceConcurrencyTests {
		if !listed[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("raceConcurrencyTests names no test: %s", strings.Join(missing, ", "))
	}
	pattern := "^(" + strings.Join(raceConcurrencyTests, "|") + ")$"
	args := []string{"go", "test", "-race", "-timeout", "30m", "-count=1", "-run", pattern}
	return append(args, slowRacePackages...), nil
}

func raceSupported() bool {
	out, err := exec.Command("go", "env", "CGO_ENABLED", "CC").Output()
	if err != nil {
		return false
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "1" {
		return false
	}
	_, err = exec.LookPath(f[1])
	return err == nil
}
