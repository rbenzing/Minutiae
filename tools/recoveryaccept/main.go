// Command recoveryaccept is the hardware-free acceptance run of every recovery command (plan 3K). It builds
// cmd/minutiae (or takes -bin) and runs it as a subprocess over gunzipped copies of the committed fixtures, inside a
// scratch directory of its own, so the exit codes and outputs are exactly what an examiner sees. It checks smoke and
// integrity, not oracle equality (the per-fixture tests do that): every command exits with its pinned code, each
// step writes artifacts of the expected kind, class and method, one case holds everything and `case verify` is
// clean, and a flipped byte in a recovered artifact makes `case verify` exit 4.
//
//	go run ./tools/recoveryaccept [-dir PARENT] [-bin BINARY] [-keep] [-verbose]
//
// Exit codes: 0 ACCEPTANCE PASSED; 1 a step failed or the run was interrupted; 2 a usage error. The tool imports
// no Minutiae package. It writes only inside the accept-* directory it creates itself (with os.Mkdir, never
// reusing a directory) and removes that directory on every exit path unless -keep is given.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
)

// modulePath is the module whose root the tool must run in.
const modulePath = "github.com/rbenzing/minutiae"

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// options are the parsed flags. root is the module root (the working directory); dir is the parent of the scratch
// directory.
type options struct {
	root, dir, bin string
	keep, verbose  bool
	// steps replaces the fixed table; only the tests set it.
	steps func() []step
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// parseFlags reads the flags. A usage error (exit 2) is returned before anything is written when go.mod is missing
// or foreign, when -dir is not inside the module root or lies under a directory named cases, or when -bin is not a
// regular file.
func parseFlags(args []string, cwd string) (options, error) {
	fs := flag.NewFlagSet("recoveryaccept", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	fs.StringVar(&o.dir, "dir", "", "parent of the scratch directory (default <root>/.claude/scratch); must be inside the project")
	fs.StringVar(&o.bin, "bin", "", "run this minutiae binary instead of building one")
	fs.BoolVar(&o.keep, "keep", false, "keep the scratch directory and print its path")
	fs.BoolVar(&o.verbose, "verbose", false, "print each command line")
	if err := fs.Parse(args); err != nil {
		return options{}, usagef("%v", err)
	}
	if fs.NArg() != 0 {
		return options{}, usagef("unexpected argument %q", fs.Arg(0))
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return options{}, usagef("working directory: %v", err)
	}
	if err := checkModule(root); err != nil {
		return options{}, err
	}
	o.root = root
	if o.dir == "" {
		o.dir = filepath.Join(root, ".claude", "scratch")
	} else if !filepath.IsAbs(o.dir) {
		o.dir = filepath.Join(root, o.dir)
	}
	o.dir = filepath.Clean(o.dir)
	if err := checkScratchParent(root, o.dir); err != nil {
		return options{}, err
	}
	if o.bin != "" {
		abs, err := filepath.Abs(o.bin)
		if err != nil {
			return options{}, usagef("-bin: %v", err)
		}
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			return options{}, usagef("-bin %q is not a regular file", o.bin)
		}
		o.bin = abs
	}
	return o, nil
}

// checkModule requires <root>/go.mod to declare this project's module.
func checkModule(root string) error {
	f, err := os.Open(filepath.Join(root, "go.mod")) //nolint:gosec // the working directory's go.mod
	if err != nil {
		return usagef("the working directory holds no go.mod: run from the Minutiae module root")
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module"); ok && strings.TrimSpace(rest) == modulePath {
			return nil
		}
	}
	return usagef("go.mod does not declare module %s: run from the Minutiae module root", modulePath)
}

// resolve makes p absolute with symlinks resolved through its deepest existing ancestor.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// checkScratchParent refuses a -dir that resolves outside root or under a directory named cases.
func checkScratchParent(root, dir string) error {
	r, err := resolve(root)
	if err != nil {
		return usagef("%v", err)
	}
	d, err := resolve(dir)
	if err != nil {
		return usagef("-dir: %v", err)
	}
	rel, err := filepath.Rel(r, d)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return usagef("-dir must be inside the project (%s)", root)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, "cases") {
			return usagef("-dir must not lie under a cases directory")
		}
	}
	return nil
}

// run is main without the process: it parses the flags and returns the exit code.
func run(ctx context.Context, args []string, cwd string, r runner, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "recoveryaccept: %v\n", err)
		var ue usageError
		if errors.As(err, &ue) {
			return exitUsage
		}
		return exitFail
	}
	return accept(ctx, o, r, stdout)
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "recoveryaccept:", err)
		os.Exit(exitUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], cwd, execRunner{}, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
