package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// block is a named group of packages that can be checked on its own.
type block struct {
	name     string
	patterns []string // module-relative, "dir/..." means dir and everything under it
}

func (b block) contains(dir string) bool {
	for _, p := range b.patterns {
		if base, ok := strings.CutSuffix(p, "/..."); ok {
			if dir == base || strings.HasPrefix(dir, base+"/") {
				return true
			}
		} else if dir == p {
			return true
		}
	}
	return false
}

// blocks is the table of check blocks. Every package of the module is in exactly one
// (TestEveryPackageIsInExactlyOneBlock).
var blocks = []block{
	{"acquisition", []string{"internal/android/...", "internal/ios/...", "internal/device/...", "internal/transport/...", "internal/protocol/..."}},
	{"image", []string{"internal/image/...", "internal/volume/...", "internal/carve/..."}},
	{"filesys", []string{"internal/filesys/..."}},
	{"sqlite", []string{"internal/sqlitefile/...", "internal/dbrecover/..."}},
	{"parsers", []string{"internal/parse/...", "internal/parsers/...", "internal/decode/...", "internal/artparse/...", "internal/recordtypes/..."}},
	{"case", []string{"internal/evidence/...", "internal/records/...", "internal/examine/...", "internal/version/..."}},
	{"cli", []string{"internal/cli/...", "cmd/..."}},
	{"tools", []string{"internal/archtest/...", "tools/..."}},
}

func blockNames() []string {
	names := make([]string, len(blocks))
	for i, b := range blocks {
		names[i] = b.name
	}
	return names
}

func blockOf(dir string) string {
	for _, b := range blocks {
		if b.contains(dir) {
			return b.name
		}
	}
	return ""
}

// docConsistencyPackages are the packages the global block tests on every affected or block run
// because they check the repository as a whole rather than one package.
var docConsistencyPackages = []struct{ dir, why string }{
	{"internal/archtest", "repo-wide source scans: architecture, purity and single-writer rules"},
	{"tools/check", "TestShortTableMatchesGuards reads docs/testing.md"},
}

// step is one command of a check run.
type step struct {
	env  []string // extra environment, KEY=VALUE
	args []string
	// prepare, when set, builds args just before the step runs; an error fails the check.
	prepare func() ([]string, error)
}

func formatStep(s step) string {
	if s.prepare != nil {
		return "(arguments computed at run time)"
	}
	return strings.Join(append(append([]string{}, s.env...), s.args...), " ")
}

func formatSteps(steps []step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = formatStep(s)
	}
	return out
}

func dirArgs(dirs []string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = "./" + d
	}
	return out
}

// globalSteps is the block that runs on every -affected and -block run.
func globalSteps() []step {
	docs := make([]string, len(docConsistencyPackages))
	for i, d := range docConsistencyPackages {
		docs[i] = d.dir
	}
	return []step{
		{args: []string{"go", "mod", "tidy", "-diff"}},
		{args: []string{"go", "run", "./tools/parserhash", "-check"}},
		{args: []string{"go", "build", "./..."}},
		scaleVet(),
		{args: []string{"go", "build", "-o", binPath(), "./cmd/minutiae"}},
		pureBuild(),
		crossBuild("linux", "amd64"),
		crossBuild("darwin", "arm64"),
		{args: append([]string{"go", "test", "-short", "-timeout", testTimeout}, dirArgs(docs)...)},
	}
}

// affectedSteps vets and lints the given package directories and tests them (-short, cached). When
// wholeModule is set the test step is the cached ./... run instead: Go's test cache records every
// file and environment variable a test consulted, so only tests that read a changed file rerun.
func affectedSteps(dirs []string, wholeModule bool) []step {
	var steps []step
	a := dirArgs(dirs)
	if len(dirs) > 0 {
		steps = append(steps,
			step{args: append([]string{"go", "vet"}, a...)},
			step{args: append([]string{"go", "tool", "golangci-lint", "run"}, a...)})
	}
	switch {
	case wholeModule:
		steps = append(steps, step{args: []string{"go", "test", "-short", "-timeout", testTimeout, "./..."}})
	case len(dirs) > 0:
		steps = append(steps, step{args: append([]string{"go", "test", "-short", "-timeout", testTimeout}, a...)})
	}
	return steps
}

// options are the command-line flags.
type options struct {
	affected bool
	base     string
	blocks   []string
	list     bool
}

// plan is the decision: what changed, what is affected and which steps run.
type plan struct {
	full        bool // the whole check runs
	reason      string
	base        string
	changed     []string
	unmapped    []string // changed files that belong to no package
	testForm    string   // which test step runs, and why
	wholeModule bool     // the test step is the cached ./... run
	affected    []string // package directories to vet, lint and test
	blocks      []string
	tested      []string // every package tested, for the summary
	steps       []step
}

// env is everything the decisions need from the outside world.
type env struct {
	git     func(args ...string) ([]byte, error)
	goList  func() ([]pkg, error)
	exists  func(string) bool
	read    func(string) ([]byte, error)
	runStep func(step) error
	race    bool
}

// usageError marks a command-line mistake (exit 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func buildPlan(o options, e env) (plan, error) {
	if o.base != "" && !o.affected {
		return plan{}, usageError{"-base needs -affected"}
	}
	if e.race && (o.affected || len(o.blocks) > 0) {
		return plan{}, usageError{"-affected and -block are not available with the race detector: the race gate is always the full form (run with CGO_ENABLED=0 or without these flags)"}
	}
	var named []block
	for _, n := range o.blocks {
		i := indexBlock(n)
		if i < 0 {
			return plan{}, usageError{fmt.Sprintf("unknown block %q; valid blocks: %s", n, strings.Join(blockNames(), ", "))}
		}
		named = append(named, blocks[i])
	}
	if !o.affected && len(named) == 0 {
		return plan{full: true, tested: []string{"all packages (./...)"}, steps: fullSteps(e.race)}, nil
	}
	pkgs, err := e.goList()
	if err != nil {
		return plan{}, err
	}
	p := plan{}
	set := map[string]bool{}
	if o.affected {
		if p.base, err = resolveBase(e.git, o.base); err != nil {
			return plan{}, err
		}
		if p.changed, err = changedFiles(e.git, p.base); err != nil {
			return plan{}, err
		}
		readBase := func(f string) ([]byte, error) {
			return e.git("show", "--end-of-options", p.base+":"+f)
		}
		hit, unmapped := changedPackages(p.changed, pkgs)
		p.unmapped = unmapped
		reason, err := escalation(p.changed, pkgs, e.exists, e.read, readBase)
		if err != nil {
			return plan{}, err
		}
		if reason != "" {
			return plan{
				full: true, reason: reason, base: p.base, changed: p.changed, unmapped: p.unmapped,
				tested: []string{"all packages (./...)"}, steps: fullSteps(e.race),
			}, nil
		}
		p.testForm = "Go files only: reverse closure of the changed packages, -short, cached"
		for _, f := range p.changed {
			if !strings.HasSuffix(f, ".go") || isTestdata(f) || slices.Contains(p.unmapped, f) {
				p.wholeModule = true
				p.testForm = "non-Go file changed: cached ./... test step (Go's test cache reruns only the tests that read a changed file)"
				break
			}
		}
		for _, d := range affectedClosure(pkgs, hit) {
			set[d] = true
		}
	}
	for _, b := range named {
		p.blocks = append(p.blocks, b.name)
		for _, q := range pkgs {
			if b.contains(q.Dir) {
				set[q.Dir] = true
			}
		}
	}
	for d := range set {
		p.affected = append(p.affected, d)
	}
	sort.Strings(p.affected)
	if o.affected {
		seen := map[string]bool{}
		for _, d := range p.affected {
			if b := blockOf(d); b != "" && !seen[b] {
				seen[b] = true
				p.blocks = append(p.blocks, b)
			}
		}
		sort.Strings(p.blocks)
	}
	p.tested = append(p.tested, p.affected...)
	if p.wholeModule {
		p.tested = append(p.tested, "all packages (./..., cached)")
	}
	for _, d := range docConsistencyPackages {
		if !set[d.dir] {
			p.tested = append(p.tested, d.dir)
		}
	}
	p.steps = append(globalSteps(), affectedSteps(p.affected, p.wholeModule)...)
	return p, nil
}

func indexBlock(name string) int {
	for i, b := range blocks {
		if b.name == name {
			return i
		}
	}
	return -1
}

func printPlan(w io.Writer, o options, p plan) {
	switch {
	case p.full && p.reason != "":
		fmt.Fprintf(w, "mode: full form (escalated from -affected: %s)\n", p.reason)
	case p.full:
		fmt.Fprintln(w, "mode: full form")
	case o.affected:
		fmt.Fprintf(w, "mode: affected (base %s)\n", p.base)
	default:
		fmt.Fprintln(w, "mode: blocks")
	}
	if len(p.changed) > 0 || o.affected {
		fmt.Fprintf(w, "changed files (%d):\n", len(p.changed))
		for _, f := range p.changed {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	if o.affected {
		fmt.Fprintln(w, "changed files mapped to no package:")
		for _, f := range p.unmapped {
			fmt.Fprintf(w, "  %s\n", f)
		}
		if len(p.unmapped) == 0 {
			fmt.Fprintln(w, "  (none)")
		}
	}
	if !p.full {
		fmt.Fprintf(w, "packages (%d):\n", len(p.affected))
		for _, d := range p.affected {
			fmt.Fprintf(w, "  %s  [%s]\n", d, blockOf(d))
		}
		fmt.Fprintf(w, "blocks: %s\n", strings.Join(p.blocks, ", "))
	}
	if p.testForm != "" {
		fmt.Fprintf(w, "tests: %s\n", p.testForm)
	}
	fmt.Fprintln(w, "steps:")
	for _, s := range formatSteps(p.steps) {
		fmt.Fprintf(w, "  %s\n", s)
	}
	fmt.Fprintf(w, "tested packages: %s\n", strings.Join(p.tested, " "))
}

// run is the command: parse flags, decide, print and (unless -list) run the steps.
func run(args []string, stdout, stderr io.Writer, e env) int {
	var o options
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.affected, "affected", false, "check only the packages affected by the changes since -base, plus the global block")
	fs.StringVar(&o.base, "base", "", "diff base for -affected (default: git merge-base HEAD main)")
	fs.Func("block", "check a named block (repeatable) plus the global block; one of "+strings.Join(blockNames(), ", "), func(s string) error {
		o.blocks = append(o.blocks, s)
		return nil
	})
	fs.BoolVar(&o.list, "list", false, "print the plan and exit without running it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}
	p, err := buildPlan(o, e)
	if err != nil {
		var ue usageError
		fmt.Fprintf(stderr, "CHECK FAILED: %v\n", err)
		if errors.As(err, &ue) {
			return 2
		}
		return 1
	}
	printPlan(stdout, o, p)
	if o.list {
		return 0
	}
	if p.full && !e.race {
		fmt.Fprintln(stdout, "note: race detector unavailable (needs cgo and a C compiler); running tests without -race")
	}
	for _, s := range p.steps {
		if s.prepare != nil {
			a, err := s.prepare()
			if err != nil {
				fmt.Fprintf(stderr, "CHECK FAILED preparing a test step: %v\n", err)
				return 1
			}
			s.args = a
			s.prepare = nil
		}
		fmt.Fprintf(stdout, "==> %s\n", formatStep(s))
		if err := e.runStep(s); err != nil {
			fmt.Fprintf(stderr, "CHECK FAILED at %q: %v\n", formatStep(s), err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "tested packages: %s\n", strings.Join(p.tested, " "))
	fmt.Fprintln(stdout, "CHECK PASSED")
	return 0
}

// slowTable reads the table of tests skipped under -short from docs/testing.md. It maps
// "<package dir> <test>" to the measured seconds.
func slowTable(md string) (map[string]int, error) {
	const heading = "Slow tests skipped under -short"
	lines := strings.Split(md, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "#") && strings.Contains(l, heading) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("docs/testing.md has no %q heading", heading)
	}
	out := map[string]int{}
	for _, l := range lines[start:] {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			break
		}
		if !strings.HasPrefix(l, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) != 3 {
			return nil, fmt.Errorf("slow-test table row %q does not have three cells", l)
		}
		test := strings.Trim(strings.TrimSpace(cells[0]), "`")
		pk := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if test == "Test" || !strings.HasPrefix(test, "Test") {
			continue // header and separator rows
		}
		secs, err := strconv.Atoi(strings.TrimSpace(cells[2]))
		if err != nil {
			return nil, fmt.Errorf("slow-test table row %q: seconds: %w", l, err)
		}
		key := pk + " " + test
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("slow-test table lists %s twice", key)
		}
		out[key] = secs
	}
	return out, nil
}

func relDir(root, dir string) (string, error) {
	r, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("package directory %s: %w", dir, err)
	}
	r = filepath.ToSlash(r)
	if r == ".." || strings.HasPrefix(r, "../") {
		return "", fmt.Errorf("package directory %s is outside the module root %s", dir, root)
	}
	return path.Clean(r), nil
}

// loadPackages lists the packages of the module in the working directory.
func loadPackages() ([]pkg, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	mod, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m: %w", err)
	}
	data, err := exec.Command("go", "list", "-json", "./...").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -json ./...: %w", err)
	}
	return parseGoList(data, strings.TrimSpace(string(mod)), root)
}

func realEnv() env {
	return env{
		git: func(args ...string) ([]byte, error) {
			out, err := exec.Command("git", args...).Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && len(ee.Stderr) > 0 {
					return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
				}
				return nil, err
			}
			return out, nil
		},
		goList: loadPackages,
		exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		read: os.ReadFile,
		runStep: func(s step) error {
			cmd := exec.Command(s.args[0], s.args[1:]...) //nolint:gosec // fixed, trusted command list
			cmd.Env = append(os.Environ(), s.env...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		race: raceSupported(),
	}
}

// isTestdata reports whether f lies under a testdata directory: such a file is data, even when it
// is named .go (the go tool ignores it, a test in another package may read it).
func isTestdata(f string) bool {
	return strings.HasPrefix(f, "testdata/") || strings.Contains(f, "/testdata/")
}

// scaleVet keeps the -tags scale harness (internal/records/scale_test.go) compiling and vetted: the
// default build never sees it and a scale run is too expensive to find a break (docs/testing.md).
func scaleVet() step {
	return step{args: []string{"go", "vet", "-tags", "scale", "./internal/records/...", "./internal/evidence/..."}}
}
