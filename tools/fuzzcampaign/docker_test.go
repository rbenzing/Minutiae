package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDockerOptions() DockerOptions {
	return DockerOptions{
		Image:       "golang:1.26.8@sha256:" + strings.Repeat("a", 64),
		Name:        "minutiae-fuzz-1760000000-4242",
		SrcCopy:     "/host/src",
		LinuxBinary: "/host/fuzzcampaign-linux",
		Out:         "/host/out",
		Args:        []string{"-profile", "decoders", "-fuzztime", "120s"},
	}
}

func TestDockerArgsAreBoundedAndUnprivileged(t *testing.T) {
	o := testDockerOptions()
	args := DockerArgs(o)
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" run --rm --name " + o.Name + " ", " --memory 4g ", " --memory-swap 4g ", " --cpus 2 ", " --pids-limit 1024 ",
		" --network none ", " -e GOPROXY=off ", " -e GOTOOLCHAIN=local ", " -e CGO_ENABLED=0 ",
		" -v /host/src:/src:ro ", " -v /host/fuzzcampaign-linux:/usr/local/bin/fuzzcampaign:ro ", " -v /host/out:/out ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the arguments lack %q:\n%s", want, joined)
		}
	}
	if len(args) < 12 {
		t.Fatalf("only %d arguments", len(args))
	}
	if args[0] != "run" {
		t.Errorf("first argument %q, want run (the caller prefixes docker)", args[0])
	}
	banned := []string{
		"--privileged", "--cap-add", "--device", "--pid=host", "--network=host", "--net=host",
		"--ipc=host", "--security-opt", "-p", "-P", "--volumes-from",
	}
	for _, flag := range banned {
		if slices.Contains(args, flag) {
			t.Errorf("the arguments contain %q", flag)
		}
	}
	for _, a := range args {
		for _, word := range []string{"losetup", "insmod", "rmmod", "depmod", "mknod", "mount", "nsenter", "modprobe"} {
			for _, w := range strings.FieldsFunc(a, func(r rune) bool { return r == ' ' || r == ';' || r == '|' || r == '&' }) {
				if w == word {
					t.Errorf("argument %q contains the word %q", a, word)
				}
			}
		}
	}
	rw := 0
	for i, a := range args {
		if a == "-v" && i+1 < len(args) && !strings.HasSuffix(args[i+1], ":ro") {
			rw++
		}
	}
	if rw != 1 {
		t.Errorf("%d read-write binds, want exactly the output directory", rw)
	}
	// The image comes before the command and the inner command is host mode on the mounts.
	i := slices.Index(args, o.Image)
	if i < 0 || i+8 > len(args) || !slices.Equal(args[i+1:i+8], []string{"fuzzcampaign", "run", "-mode", "host", "-src", "/src", "-out"}) {
		t.Errorf("after the image: %q", args[i+1:])
	}
	if !slices.Equal(args[len(args)-4:], o.Args) {
		t.Errorf("the inner arguments are not last: %q", args[len(args)-4:])
	}
	// Caches live inside the output bind: no named volume and no extra mount.
	if !strings.Contains(joined, " -e GOMODCACHE=/out/.cache/gomod ") || !strings.Contains(joined, " -e GOCACHE=/out/.cache/go-build ") {
		t.Errorf("the caches are not placed under the output directory:\n%s", joined)
	}
	if strings.Contains(joined, "minutiae-fuzz-gomod") || strings.Contains(joined, "minutiae-fuzz-cache") {
		t.Errorf("a named volume is used")
	}
}

func TestDockerPrepareArgsDownloadOnlyWhatThePackagesNeed(t *testing.T) {
	o := testDockerOptions()
	args := DockerPrepareArgs(o, []string{"./internal/decode/plist", "./internal/decode/ts"})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--network none") || strings.Contains(joined, "GOPROXY=off") {
		t.Errorf("prepare must have the network:\n%s", joined)
	}
	wants := []string{
		"go list -deps -test", "not .Module.Main", "{{.Module.Path}}@{{.Module.Version}}",
		"./internal/decode/plist ./internal/decode/ts", "sort -u", "xargs -r go mod download",
		"/host/src:/src:ro", "-v /host/out:/out", "GOFLAGS=-modcacherw",
	}
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Errorf("the prepare command lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "{{.Path}}") {
		t.Errorf("the template uses {{.Path}}, which is not a package field")
	}
	if regexp.MustCompile(`(^|[;&|] *)go mod download`).MatchString(joined) {
		t.Errorf("a bare go mod download would fetch every requirement:\n%s", joined)
	}
	// The template, run on this repository for these packages, names no main module and no
	// entry without a version.
	out, err := exec.Command("go", "list", "-deps", "-test", "-f",
		"{{if and .Module (not .Module.Main)}}{{.Module.Path}}@{{.Module.Version}}{{end}}",
		"../../internal/decode/plist", "../../internal/decode/ts").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "GOPROXY=off") {
			t.Skipf("the template run needs the module cache, which this offline environment does not have: %s", ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, "github.com/rbenzing/minutiae") {
			t.Errorf("the main module is listed: %s", line)
		}
		if strings.HasSuffix(line, "@") || !strings.Contains(line, "@v") {
			t.Errorf("an entry without a version: %q", line)
		}
	}
}

func TestDockerNameHasNoUserText(t *testing.T) {
	now := time.Unix(1760000000, 0)
	n := DockerName(now, 4242)
	if !regexp.MustCompile(`^minutiae-fuzz-[0-9]+-[0-9]+$`).MatchString(n) {
		t.Errorf("name %q is not built from numbers only", n)
	}
	if DockerName(now, 4242) == DockerName(now, 4243) {
		t.Errorf("two pids in one second gave the same name")
	}
}

// fakeExec records docker calls; Run blocks until the context ends when block is set.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
	block bool
	code  int
	err   error
}

func (f *fakeExec) Run(ctx context.Context, name string, args ...string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	isRun := len(args) > 0 && args[0] == "run"
	f.mu.Unlock()
	if isRun && f.block {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if isRun {
		return f.code, f.err
	}
	return 0, nil
}

func (f *fakeExec) cleanupCalls() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 1 && (c[1] == "kill" || c[1] == "rm") {
			out = append(out, c)
		}
	}
	return out
}

func TestDockerModeKillsTheNamedContainerOnDeadline(t *testing.T) {
	o := testDockerOptions()
	want := [][]string{
		append([]string{"docker"}, DockerKillArgs(o.Name)...),
		append([]string{"docker"}, DockerRemoveArgs(o.Name)...),
	}
	if !slices.Equal(DockerKillArgs(o.Name), []string{"kill", o.Name}) || !slices.Equal(DockerRemoveArgs(o.Name), []string{"rm", "-f", o.Name}) {
		t.Fatalf("kill %q remove %q", DockerKillArgs(o.Name), DockerRemoveArgs(o.Name))
	}

	t.Run("deadline reached", func(t *testing.T) {
		f := &fakeExec{block: true}
		start := time.Now()
		code, cause := RunContainer(context.Background(), f, o, 200*time.Millisecond)
		if code != 1 || cause == "" {
			t.Errorf("exit %d cause %q, want 1 and a cause", code, cause)
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		if got := f.cleanupCalls(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("cleanup calls %v, want exactly %v", got, want)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		f := &fakeExec{block: true}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		code, _ := RunContainer(ctx, f, o, time.Hour)
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if got := f.cleanupCalls(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("cleanup calls %v, want exactly %v", got, want)
		}
	})
	t.Run("early error", func(t *testing.T) {
		f := &fakeExec{code: -1, err: os.ErrNotExist}
		code, cause := RunContainer(context.Background(), f, o, time.Hour)
		if code != 1 || cause == "" {
			t.Errorf("exit %d cause %q", code, cause)
		}
		if got := f.cleanupCalls(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("cleanup calls %v, want exactly %v", got, want)
		}
	})
	t.Run("normal exit also cleans up once", func(t *testing.T) {
		f := &fakeExec{code: 0}
		code, _ := RunContainer(context.Background(), f, o, time.Hour)
		if code != 0 {
			t.Errorf("exit %d", code)
		}
		if got := f.cleanupCalls(); len(got) != 2 {
			t.Errorf("cleanup calls %v, want kill and rm once each", got)
		}
	})
}

func TestDockerExitCodeMapping(t *testing.T) {
	for _, code := range []int{125, 126, 127, 137} {
		got, cause := MapDockerExit(code)
		if got != 1 || cause == "" {
			t.Errorf("docker exit %d -> %d, cause %q; want 1 with a cause", code, got, cause)
		}
	}
	for _, code := range []int{0, 1, 2} {
		if got, _ := MapDockerExit(code); got != code {
			t.Errorf("exit %d mapped to %d", code, got)
		}
	}
	if got, cause := MapDockerExit(3); got != 1 || cause == "" {
		t.Errorf("an unknown exit 3 -> %d, %q; want 1 with a cause", got, cause)
	}
}

func TestDockerfilePinsTheBaseImage(t *testing.T) {
	df, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var froms []string
	for _, l := range strings.Split(string(df), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "FROM ") {
			froms = append(froms, strings.TrimSpace(l))
		}
	}
	if len(froms) != 1 {
		t.Fatalf("%d FROM lines, want 1", len(froms))
	}
	m := regexp.MustCompile(`^FROM golang:([0-9][0-9.]*)@sha256:[0-9a-f]{64}$`).FindStringSubmatch(froms[0])
	if m == nil {
		t.Fatalf("FROM line %q is not golang:<version>@sha256:<64 hex>", froms[0])
	}
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	tm := regexp.MustCompile(`(?m)^toolchain go([0-9][0-9.]*)$`).FindStringSubmatch(string(mod))
	if tm == nil || tm[1] != m[1] {
		t.Errorf("Dockerfile Go %s, go.mod toolchain %v: bump the digest in the same commit as the toolchain", m[1], tm)
	}
	if strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(df), froms[0], ""), "\n", "")) != "" &&
		strings.Contains(strings.ReplaceAll(string(df), froms[0], ""), "RUN") {
		t.Errorf("the Dockerfile holds more than the pinned FROM")
	}
}

func TestCheckImageGoVersionRefusesAMismatch(t *testing.T) {
	mod := "module x\n\ngo 1.26.0\n\ntoolchain go1.26.8\n"
	if err := CheckImageGoVersion("go version go1.26.8 linux/amd64\n", mod); err != nil {
		t.Errorf("a matching version was refused: %v", err)
	}
	for _, bad := range []string{"go version go1.26.7 linux/amd64", "garbage", "", "go version go1.26.8 windows/amd64"} {
		if err := CheckImageGoVersion(bad, mod); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if err := CheckImageGoVersion("go version go1.26.8 linux/amd64", "module x\n"); err == nil {
		t.Errorf("a go.mod without a toolchain line was accepted")
	}
}
