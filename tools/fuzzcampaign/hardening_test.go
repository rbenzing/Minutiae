package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// V63: user text must never become a docker option.
func TestImageNameCannotBecomeADockerOption(t *testing.T) {
	old := dockerOutput
	dockerOutput = func(context.Context, string, ...string) (string, error) {
		t.Errorf("docker was invoked with an image that looks like an option")
		return "", errors.New("must not run")
	}
	t.Cleanup(func() { dockerOutput = old })
	out := filepath.Join(t.TempDir(), "out")
	for name, args := range map[string][]string{
		"run privileged":     {"run", "-out", out, "-mode", "docker", "-image", "--privileged", "-linux-binary", "x"},
		"run short option":   {"run", "-out", out, "-mode", "docker", "-image", "-v", "-linux-binary", "x"},
		"prepare privileged": {"prepare", "-out", out, "-image", "--privileged"},
	} {
		if code, _, stderr := runMain(t, args...); code != 2 || !strings.Contains(stderr, "-image") {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming -image", name, code, stderr)
		}
	}
	for _, ok := range []string{"minutiae-fuzz:1", "golang:1.26.8@sha256:" + strings.Repeat("a", 64), "registry.example/x/y:2"} {
		if !imageRE.MatchString(ok) {
			t.Errorf("image %q was refused", ok)
		}
	}
	for _, bad := range []string{"-x", "--privileged", "", "a b", "a;b"} {
		if imageRE.MatchString(bad) {
			t.Errorf("image %q was accepted", bad)
		}
	}
}

// recordingOutput records the docker calls of the version check; Run blocks until the context
// ends when block is set.
type recordingOutput struct {
	mu    sync.Mutex
	calls [][]string
	block bool
}

func (r *recordingOutput) run(ctx context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if len(args) > 0 && args[0] == "run" {
		if r.block {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "go version go1.26.8 linux/amd64\n", nil
	}
	return "", nil
}

// V64: the go version check runs like a target: bounded, offline, named, killed by name.
func TestImageGoVersionRunsLikeABoundedNamedTarget(t *testing.T) {
	const image, name = "minutiae-fuzz:1", "minutiae-fuzz-check-1760000000-4242"
	args := DockerVersionArgs(image, name)
	for _, want := range []string{"--rm", "--network", "none", "--name", name, "--memory", "--pids-limit"} {
		if !slices.Contains(args, want) {
			t.Errorf("version args %q lack %q", args, want)
		}
	}
	for _, bad := range []string{"--privileged", "-v", "--volume", "--cap-add", "--device", "-p"} {
		if slices.Contains(args, bad) {
			t.Errorf("version args %q contain %q", args, bad)
		}
	}
	if n := len(args); n < 3 || !slices.Equal(args[n-3:], []string{image, "go", "version"}) {
		t.Errorf("version args %q must end with the image and the command", args)
	}
	if n := VersionContainerName(time.Unix(1760000000, 0), 4242); !regexp.MustCompile(`^minutiae-fuzz-check-[0-9]+-[0-9]+$`).MatchString(n) {
		t.Errorf("name %q is not built from numbers only", n)
	}

	wantCleanup := [][]string{
		append([]string{"docker"}, DockerKillArgs(name)...),
		append([]string{"docker"}, DockerRemoveArgs(name)...),
	}
	cleanup := func(r *recordingOutput) [][]string {
		var out [][]string
		for _, c := range r.calls {
			if len(c) > 1 && (c[1] == "kill" || c[1] == "rm") {
				out = append(out, c)
			}
		}
		return out
	}
	old := dockerOutput
	t.Cleanup(func() { dockerOutput = old })

	t.Run("normal", func(t *testing.T) {
		r := &recordingOutput{}
		dockerOutput = r.run
		out, err := ImageGoVersion(context.Background(), image, name, time.Minute)
		if err != nil || !strings.Contains(out, "go1.26.8") {
			t.Errorf("out %q err %v", out, err)
		}
		if got := cleanup(r); !slices.EqualFunc(got, wantCleanup, slices.Equal[[]string]) {
			t.Errorf("cleanup %v, want %v", got, wantCleanup)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		r := &recordingOutput{block: true}
		dockerOutput = r.run
		start := time.Now()
		if _, err := ImageGoVersion(context.Background(), image, name, 100*time.Millisecond); err == nil {
			t.Errorf("a version check that hit its deadline was not an error")
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		if got := cleanup(r); !slices.EqualFunc(got, wantCleanup, slices.Equal[[]string]) {
			t.Errorf("cleanup %v, want %v", got, wantCleanup)
		}
	})
	t.Run("interrupt", func(t *testing.T) {
		r := &recordingOutput{block: true}
		dockerOutput = r.run
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		if _, err := ImageGoVersion(ctx, image, name, time.Hour); err == nil {
			t.Errorf("an interrupted version check was not an error")
		}
		if got := cleanup(r); !slices.EqualFunc(got, wantCleanup, slices.Equal[[]string]) {
			t.Errorf("cleanup %v, want %v", got, wantCleanup)
		}
	})
}

// V65: a target that reported new interesting inputs has a cache directory; a missing one is
// unknown, not "nothing to prune".
func TestMissingCorpusDirWithNewInterestingFailsTheTarget(t *testing.T) {
	env := testEnv(t, map[string]string{"HELPER_CAT": progressFile(t, "fuzzing.txt")}, nil) // new interesting 43
	if err := os.RemoveAll(env.CacheDir(testItem.Target)); err != nil {
		t.Fatal(err)
	}
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewInteresting == 0 {
		t.Fatalf("the fixture reports no new interesting input")
	}
	if res.Outcome == OK || !strings.Contains(res.Note, "cache") {
		t.Errorf("outcome %q note %q; a missing cache directory after new interesting inputs must fail the target", res.Outcome, res.Note)
	}
}

func TestMissingCorpusDirWithoutNewInterestingIsNothingToPrune(t *testing.T) {
	env := testEnv(t, nil, nil)
	if err := os.RemoveAll(env.CacheDir(testItem.Target)); err != nil {
		t.Fatal(err)
	}
	if note := pruneCorpus(env, testItem.Target, 0); note != "" {
		t.Errorf("a missing cache directory with no new interesting input produced the note %q", note)
	}
}

func TestDefaultCacheDirHonoursTheCampaignEnvironment(t *testing.T) {
	work, cache := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.com/m\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := defaultCacheDir(work, []string{"GOCACHE=" + cache, "PATH=" + os.Getenv("PATH")}, Target{Pkg: "internal/x", Name: "FuzzX"})
	want := filepath.Join(cache, "fuzz", "example.com", "m", "internal", "x", "FuzzX")
	if got != want {
		t.Errorf("cache dir %q, want %q", got, want)
	}
}

// V66: results.json is rewritten atomically after each target, and a write error is an error.
func TestCampaignReportsAfterEachTarget(t *testing.T) {
	var seen []int
	env := Env{OnResult: func(rs []RunResult) { seen = append(seen, len(rs)) }}
	_, code := Campaign(context.Background(), planOf(3), &fakeRunner{}, env, time.Now().Add(24*time.Hour))
	if code != 0 || !slices.Equal(seen, []int{1, 2, 3}) {
		t.Errorf("exit %d, results seen after each target %v; want 0 and [1 2 3]", code, seen)
	}
}

func TestWriteResultsFileIsAtomicAndReportsErrors(t *testing.T) {
	dir := t.TempDir()
	rs := []RunResult{{Target: Target{Pkg: "p", Name: "FuzzA"}, Outcome: OK}}
	if err := WriteResultsFile(dir, rs); err != nil {
		t.Fatal(err)
	}
	rs = append(rs, RunResult{Target: Target{Pkg: "p", Name: "FuzzB"}, Outcome: Failed})
	if err := WriteResultsFile(dir, rs); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseResultsJSON(b)
	if err != nil || len(got) != 2 {
		t.Errorf("results.json holds %d results (err %v), want 2", len(got), err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("the directory holds %d entries after the rewrite, want only results.json", len(es))
	}
	if err := WriteResultsFile(filepath.Join(dir, "no", "such"), rs); err == nil {
		t.Errorf("a write into a missing directory was not an error")
	}
}

// V67: after a cancel the remaining targets are skipped and never started.
func TestCampaignSkipsTheRemainingTargetsAfterACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{tick: cancel}
	res, code := Campaign(ctx, planOf(4), r, Env{}, time.Now().Add(24*time.Hour))
	if len(res) != 4 || code != 1 {
		t.Fatalf("%d results, exit %d; want 4 and 1", len(res), code)
	}
	if len(r.ran) != 1 {
		t.Errorf("ran %v after the cancel, want only the first target", r.ran)
	}
	for _, i := range []int{1, 2, 3} {
		if res[i].Outcome != Skipped || !strings.Contains(res[i].Note, "cancelled") {
			t.Errorf("result %d = %+v, want skipped with the note cancelled", i, res[i])
		}
	}
}

// V68: a copy is bounded by the size it checked and by a file count.
func TestCopyFileLimitedRefusesAFileThatChangedSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	if err := os.WriteFile(src, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileLimited(src, filepath.Join(dir, "same"), 10); err != nil {
		t.Errorf("a file of the checked size: %v", err)
	}
	if err := copyFileLimited(src, filepath.Join(dir, "grew"), 4); !errors.Is(err, errCopySizeChanged) {
		t.Errorf("a file that grew past its checked size: err %v, want errCopySizeChanged", err)
	}
	if err := copyFileLimited(src, filepath.Join(dir, "shrank"), 20); !errors.Is(err, errCopySizeChanged) {
		t.Errorf("a file that shrank below its checked size: err %v, want errCopySizeChanged", err)
	}
}

func TestCopyRefusesTooManyFiles(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a": "", "b": "", "c": "", "d": ""})
	old := maxCopyFiles
	maxCopyFiles = 3
	t.Cleanup(func() { maxCopyFiles = old })
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy")); !errors.Is(err, errTooManyFiles) {
		t.Errorf("err = %v, want errTooManyFiles for four empty files over a cap of three", err)
	}
	maxCopyFiles = 4
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy2")); err != nil {
		t.Errorf("a copy exactly at the file cap: %v", err)
	}
}

// V69: an -out that reaches the source or a cases directory through a link is refused.
func TestOutDirThroughASymlinkIsRefused(t *testing.T) {
	src, base := t.TempDir(), t.TempDir()
	cases := filepath.Join(base, "cases")
	if err := os.MkdirAll(cases, 0o750); err != nil {
		t.Fatal(err)
	}
	toSrc, toCases := filepath.Join(base, "to-src"), filepath.Join(base, "to-cases")
	if err := os.Symlink(src, toSrc); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.Symlink(cases, toCases); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	work := filepath.Join(t.TempDir(), "work")
	for _, out := range []string{toSrc, filepath.Join(toSrc, "new", "deeper"), toCases, filepath.Join(toCases, "x")} {
		if err := CheckOutDir(out, src, work); err == nil {
			t.Errorf("-out %q reaches the source or a cases directory through a link and was accepted", out)
		}
	}
	if err := CheckOutDir(filepath.Join(base, "fine"), src, work); err != nil {
		t.Errorf("a plain sibling directory was refused: %v", err)
	}
}

// V64: the docker mode reaches the image only through the bounded, named version check.
func TestDockerModeChecksTheImageThroughTheBoundedCheck(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n\ngo 1.26\n\ntoolchain go1.26.8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "p"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := "package p\n\nimport \"testing\"\n\nfunc FuzzA(f *testing.F) {}\n"
	if err := os.WriteFile(filepath.Join(src, "p", "p_test.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordingOutput{}
	old := dockerOutput
	dockerOutput = func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "run" {
			_, _ = r.run(ctx, name, args...)
			return "go version go1.99.0 linux/amd64\n", nil // a mismatch: the run stops here
		}
		return r.run(ctx, name, args...)
	}
	t.Cleanup(func() { dockerOutput = old })
	out := filepath.Join(t.TempDir(), "out")
	code, _, stderr := runMain(t, "run", "-mode", "docker", "-image", "minutiae-fuzz:1", "-linux-binary", "x", "-src", src, "-pkg", "p", "-out", out)
	if code != 1 || !strings.Contains(stderr, "1.99.0") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the mismatch", code, stderr)
	}
	if len(r.calls) == 0 || !slices.Contains(r.calls[0], "--network") || !slices.Contains(r.calls[0], "--name") {
		t.Errorf("the image check ran as %v; want a named container with --network none", r.calls)
	}
	var kills int
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == "kill" {
			kills++
		}
	}
	if kills != 1 {
		t.Errorf("%d kill calls, want 1 (cleanup by name)", kills)
	}
}

// V72: the file-count cap is its own error, not the "over 2 GiB" one.
func TestFileCapIsNotTheByteCapError(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"a": "", "b": "", "c": ""})
	old := maxCopyFiles
	maxCopyFiles = 2
	t.Cleanup(func() { maxCopyFiles = old })
	_, err := CopyTree(src, filepath.Join(t.TempDir(), "copy"))
	if !errors.Is(err, errTooManyFiles) || errors.Is(err, errCopyTooBig) {
		t.Fatalf("err = %v, want errTooManyFiles and not errCopyTooBig", err)
	}
	if strings.Contains(err.Error(), "2 GiB") {
		t.Errorf("the file-count error talks about bytes: %v", err)
	}
}

// V74: directories count toward the cap in the non-git walk.
func TestCopyCapCountsDirectories(t *testing.T) {
	src := t.TempDir()
	for _, d := range []string{"d1", "d2", "d3", "d4"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	old := maxCopyFiles
	maxCopyFiles = 3
	t.Cleanup(func() { maxCopyFiles = old })
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy")); !errors.Is(err, errTooManyFiles) {
		t.Errorf("err = %v, want errTooManyFiles for four empty directories over a cap of three", err)
	}
	maxCopyFiles = 4
	if _, err := CopyTree(src, filepath.Join(t.TempDir(), "copy2")); err != nil {
		t.Errorf("four directories at a cap of four: %v", err)
	}
}

// V73: a failed write leaves no temporary file behind, and an empty plan is refused before
// any run (so there is never a campaign without a results.json).
func TestWriteResultsFileRemovesItsTempFileOnAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	old := writeFile
	writeFile = func(name string, b []byte, perm os.FileMode) error {
		_ = os.WriteFile(name, b[:len(b)/2], perm)
		return errors.New("disk full")
	}
	t.Cleanup(func() { writeFile = old })
	if err := WriteResultsFile(dir, []RunResult{{Target: Target{Pkg: "p", Name: "FuzzA"}, Outcome: OK}}); err == nil {
		t.Fatal("a failed write was not an error")
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Errorf("%d entries left after a failed write, want none", len(es))
	}
	if _, err := BuildPlan(nil, DefaultOptions("all")); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("an empty plan is not refused before the run: %v", err)
	}
}
