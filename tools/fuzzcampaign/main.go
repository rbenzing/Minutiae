// Command fuzzcampaign plans and runs bounded fuzzing campaigns over every fuzz target in the
// repository.
//
//	fuzzcampaign plan [-list] [flags]       print the plan and its estimate (or only the selection)
//	fuzzcampaign run -out DIR [flags]       run the campaign (-mode host|docker)
//	fuzzcampaign prepare -image I -out DIR  download the modules the selected packages need (docker)
//
// It runs fuzzers only in a throwaway copy of the source, never in the working tree, and in
// docker mode only with a host-made copy that excludes cases/ and every untracked file. Exit
// codes: 0 every target ok; 1 any other outcome, a finding, a skipped target, the campaign
// deadline or a docker failure; 2 a usage error. See docs/testing.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	// imageRE must not allow a leading "-": the image is placed where docker parses options, and `docker run` has no "--" to end them.
	imageRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@/-]*$`)
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type cliOpts struct {
	profile                   string
	pkgs                      multiFlag
	fuzztime, minimize, total time.Duration
	parallel                  int
	src, out, mode            string
	image, linuxBinary        string
	list                      bool
}

func newFlags(name string, stderr io.Writer, o *cliOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("fuzzcampaign "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.profile, "profile", "all", "target profile: decoders, readers, parsers or all")
	fs.Var(&o.pkgs, "pkg", "package path prefix narrowing the profile (repeatable)")
	fs.DurationVar(&o.fuzztime, "fuzztime", 120*time.Second, "fuzz time per target (10s to 15m)")
	fs.DurationVar(&o.minimize, "minimize", 10*time.Second, "minimisation time (0 to 60s)")
	fs.DurationVar(&o.total, "total", 90*time.Minute, "cap on the whole campaign (10m to 240m)")
	fs.IntVar(&o.parallel, "parallel", 2, "fuzz workers per target (1 to 4)")
	fs.StringVar(&o.src, "src", ".", "the source tree (default the current directory)")
	return fs
}

func (o *cliOpts) planOptions() PlanOptions {
	return PlanOptions{Profile: o.profile, Pkgs: o.normalisedPkgs(), PerTarget: o.fuzztime, Minimize: o.minimize, Total: o.total, Parallel: o.parallel}
}

func (o *cliOpts) normalisedPkgs() []string {
	var out []string
	for _, p := range o.pkgs {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/"))
	}
	return out
}

func (o *cliOpts) validNames() error {
	if !nameRE.MatchString(o.profile) {
		return fmt.Errorf("invalid -profile %q", o.profile)
	}
	for _, p := range o.pkgs {
		if !nameRE.MatchString(p) {
			return fmt.Errorf("invalid -pkg %q: only letters, digits and _ . / - are allowed", p)
		}
	}
	return nil
}

// absPath is filepath.Abs, except that on Windows a path rooted without a drive letter (the Git
// Bash form /c/work) is refused: Abs would silently put it on the current drive.
func absPath(p string) (string, error) {
	if runtime.GOOS == "windows" && filepath.VolumeName(p) == "" && p != "" && (p[0] == '/' || p[0] == '\\') {
		return "", fmt.Errorf("%q has no drive letter: use a Windows path such as C:\\work\\out", p)
	}
	return filepath.Abs(p)
}

func usage(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "fuzzcampaign: "+format+"\n", a...)
	return 2
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr, "a subcommand is required: plan, run or prepare")
	}
	var o cliOpts
	switch args[0] {
	case "plan":
		fs := newFlags("plan", stderr, &o)
		fs.BoolVar(&o.list, "list", false, "print the selection only, with no cap check")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return cmdPlan(&o, stdout, stderr)
	case "run":
		fs := newFlags("run", stderr, &o)
		fs.StringVar(&o.out, "out", "", "campaign output directory (required)")
		fs.StringVar(&o.mode, "mode", "host", "host or docker")
		fs.StringVar(&o.image, "image", "", "docker image (docker mode)")
		fs.StringVar(&o.linuxBinary, "linux-binary", "", "linux/amd64 build of this tool (docker mode)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return cmdRun(&o, stdout, stderr)
	case "prepare":
		fs := newFlags("prepare", stderr, &o)
		fs.StringVar(&o.out, "out", "", "output directory that holds the module cache (required)")
		fs.StringVar(&o.image, "image", "", "docker image (required)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		return cmdPrepare(&o, stdout, stderr)
	}
	return usage(stderr, "unknown subcommand %q: plan, run or prepare", args[0])
}

func discover(o *cliOpts) ([]Target, error) {
	ts, err := Discover(o.src)
	if err != nil {
		return nil, err
	}
	return ts, nil
}

func cmdPlan(o *cliOpts, stdout, stderr io.Writer) int {
	if err := o.validNames(); err != nil {
		return usage(stderr, "%v", err)
	}
	ts, err := discover(o)
	if err != nil {
		return usage(stderr, "%v", err)
	}
	if o.list {
		sel, err := List(ts, o.planOptions())
		if err != nil {
			return usage(stderr, "%v", err)
		}
		for _, t := range sel {
			fmt.Fprintf(stdout, "%s.%s\n", t.Pkg, t.Name)
		}
		return 0
	}
	p, err := BuildPlan(ts, o.planOptions())
	if err != nil {
		return usage(stderr, "%v", err)
	}
	for _, it := range p.Items {
		fmt.Fprintf(stdout, "%s.%s  fuzztime=%s minimize=%s\n", it.Pkg, it.Name, it.Fuzztime, it.Minimize)
	}
	fmt.Fprintf(stdout, "%d target(s), estimate %s (cap %s)\n", len(p.Items), p.Estimate, p.Options.Total)
	return 0
}

// dockerOutput runs a command and returns its output; a variable so a test can replace it.
var dockerOutput = func(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed argument vector built from validated names
	return string(out), err
}

func cmdRun(o *cliOpts, stdout, stderr io.Writer) int {
	if o.out == "" {
		return usage(stderr, "-out is required")
	}
	if err := o.validNames(); err != nil {
		return usage(stderr, "%v", err)
	}
	if o.mode != "host" && o.mode != "docker" {
		return usage(stderr, "unknown -mode %q: host or docker", o.mode)
	}
	if o.mode == "docker" && (o.image == "" || o.linuxBinary == "") {
		return usage(stderr, "-mode docker needs -image and -linux-binary")
	}
	if o.image != "" && !imageRE.MatchString(o.image) {
		return usage(stderr, "invalid -image %q", o.image)
	}
	out, err := absPath(o.out)
	if err != nil {
		return usage(stderr, "-out: %v", err)
	}
	src, err := absPath(o.src)
	if err != nil {
		return usage(stderr, "-src: %v", err)
	}
	work := filepath.Join(out, "work")
	if err := CheckOutDir(out, src, work); err != nil {
		return usage(stderr, "-out: %v", err)
	}
	ts, err := Discover(src)
	if err != nil {
		return usage(stderr, "%v", err)
	}
	plan, err := BuildPlan(ts, o.planOptions())
	if err != nil {
		return usage(stderr, "%v", err)
	}
	if o.mode == "host" {
		if err := HostModeAllowed(runtime.GOOS); err != nil {
			return usage(stderr, "%v", err)
		}
		return runHost(out, src, work, plan, o, stdout, stderr)
	}
	return runDockerMode(out, src, o, stdout, stderr)
}

func runHost(out, src, work string, plan Plan, o *cliOpts, stdout, stderr io.Writer) int {
	if err := os.MkdirAll(out, 0o750); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: %v\n", err)
		return 1
	}
	if _, err := CopyTree(src, work); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: copying the source: %v\n", err)
		return 1
	}
	env := Env{Work: work, Out: out, Parallel: o.parallel}
	env.CorpusMax.Files, env.CorpusMax.Bytes = 2000, 64<<20
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var writeErr error
	env.OnResult = func(rs []RunResult) {
		if err := WriteResultsFile(out, rs); err != nil && writeErr == nil {
			writeErr = err
		}
	}
	results, code := Campaign(ctx, plan, GoRunner{}, env, nowFunc().Add(o.total))
	fmt.Fprint(stdout, Summary(results))
	if writeErr != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: results.json could not be written: %v\n", writeErr)
		return 1
	}
	return code
}

func runDockerMode(out, src string, o *cliOpts, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mod, err := os.ReadFile(filepath.Join(src, "go.mod"))
	if err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: %v\n", err)
		return 1
	}
	ver, err := ImageGoVersion(ctx, o.image, VersionContainerName(nowFunc(), os.Getpid()), time.Minute)
	if err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: checking the image: %v\n", err)
		return 1
	}
	if err := CheckImageGoVersion(ver, string(mod)); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	binary, err := absPath(o.linuxBinary)
	if err != nil {
		return usage(stderr, "-linux-binary: %v", err)
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: %v\n", err)
		return 1
	}
	srcCopy := filepath.Join(out, "src")
	if _, err := CopyTree(src, srcCopy); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: copying the source: %v\n", err)
		return 1
	}
	opts := DockerOptions{
		Image: o.image, Name: DockerName(nowFunc(), os.Getpid()),
		SrcCopy: srcCopy, LinuxBinary: binary, Out: out, Args: o.innerArgs(),
	}
	fmt.Fprintf(stdout, "container %s\n", opts.Name)
	code, cause := RunContainer(ctx, osExec{}, opts, o.total+10*time.Minute)
	if b, err := os.ReadFile(filepath.Join(out, "results.json")); err == nil {
		if rs, err := ParseResultsJSON(b); err == nil {
			fmt.Fprint(stdout, Summary(rs))
		}
	}
	if cause != "" {
		fmt.Fprintf(stderr, "fuzzcampaign: %s\n", cause)
	}
	return code
}

// innerArgs are the campaign flags handed to the tool inside the container.
func (o *cliOpts) innerArgs() []string {
	a := []string{
		"-profile", o.profile, "-fuzztime", o.fuzztime.String(), "-minimize", o.minimize.String(),
		"-total", o.total.String(), "-parallel", fmt.Sprint(o.parallel),
	}
	for _, p := range o.normalisedPkgs() {
		a = append(a, "-pkg", p)
	}
	return a
}

func cmdPrepare(o *cliOpts, stdout, stderr io.Writer) int {
	if o.image == "" || o.out == "" {
		return usage(stderr, "prepare needs -image and -out")
	}
	if err := o.validNames(); err != nil {
		return usage(stderr, "%v", err)
	}
	if !imageRE.MatchString(o.image) {
		return usage(stderr, "invalid -image %q", o.image)
	}
	out, err := absPath(o.out)
	if err != nil {
		return usage(stderr, "-out: %v", err)
	}
	src, err := absPath(o.src)
	if err != nil {
		return usage(stderr, "-src: %v", err)
	}
	if err := CheckOutDir(out, src, filepath.Join(out, "work")); err != nil {
		return usage(stderr, "-out: %v", err)
	}
	ts, err := Discover(src)
	if err != nil {
		return usage(stderr, "%v", err)
	}
	sel, err := List(ts, o.planOptions())
	if err != nil {
		return usage(stderr, "%v", err)
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, t := range sel {
		if p := "./" + t.Pkg; !seen[p] {
			seen[p] = true
			pkgs = append(pkgs, p)
		}
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: %v\n", err)
		return 1
	}
	srcCopy := filepath.Join(out, "prepare-src")
	if _, err := CopyTree(src, srcCopy); err != nil {
		fmt.Fprintf(stderr, "fuzzcampaign: copying the source: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(srcCopy) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := DockerOptions{Image: o.image, Name: DockerName(nowFunc(), os.Getpid()), SrcCopy: srcCopy, Out: out}
	fmt.Fprintf(stdout, "container %s: downloading the modules of %d package(s)\n", opts.Name, len(pkgs))
	code, cause := runDocker(ctx, osExec{}, opts.Name, DockerPrepareArgs(opts, pkgs), 30*time.Minute)
	if cause != "" {
		fmt.Fprintf(stderr, "fuzzcampaign: %s\n", cause)
	}
	if code == 0 {
		fmt.Fprintf(stdout, "the module cache in %s now supports the profile %q\n", filepath.Join(out, ".cache", "gomod"), o.profile)
	}
	return code
}
