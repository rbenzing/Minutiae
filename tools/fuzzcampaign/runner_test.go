package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake command is this test binary re-invoked with scripted behaviour in the environment
// (the standard TestHelperProcess pattern).
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("FUZZCAMPAIGN_HELPER") != "1" {
		return
	}
	if p := os.Getenv("HELPER_CAT"); p != "" {
		b, err := os.ReadFile(p) //nolint:gosec // test helper: the path is the test's own
		if err != nil {
			os.Exit(99)
		}
		_, _ = os.Stdout.Write(b)
	}
	if s := os.Getenv("HELPER_PRINT"); s != "" {
		_, _ = os.Stdout.WriteString(s + "\n")
	}
	if n, _ := strconv.Atoi(os.Getenv("HELPER_LINES")); n > 0 {
		var sb strings.Builder
		for i := 1; i <= n; i++ {
			sb.WriteString("fuzz: elapsed: " + strconv.Itoa(i) + "s, execs: " + strconv.Itoa(i) + " (1/sec), new interesting: 0 (total: 1)\n")
		}
		_, _ = os.Stdout.WriteString(sb.String())
	}
	if n, _ := strconv.Atoi(os.Getenv("HELPER_BIGLINE")); n > 0 {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", n) + "\n")
		_, _ = os.Stdout.WriteString("fuzz: elapsed: 3s, execs: 7 (1/sec), new interesting: 0 (total: 1)\n")
	}
	if p := os.Getenv("HELPER_CREATE"); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0o750)                                             //nolint:gosec // test helper
		_ = os.WriteFile(p, []byte{0x00, 0xff, 'b', 'y', 't', 'e', 's', '\r', '\n'}, 0o600) //nolint:gosec // test helper
	}
	if p := os.Getenv("HELPER_SPAWN"); p != "" {
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // re-invokes the test binary
		child.Env = append(os.Environ(), "HELPER_SPAWN=", "HELPER_SLEEP=120", "HELPER_PIDFILE=")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(p, []byte(strconv.Itoa(child.Process.Pid)), 0o600) //nolint:gosec // test helper
		}
	}
	if s, _ := strconv.Atoi(os.Getenv("HELPER_SLEEP")); s > 0 {
		time.Sleep(time.Duration(s) * time.Second)
	}
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

// helper returns a Command seam that runs the helper process with the given script variables,
// and records what the runner asked for.
type recorded struct {
	dir  string
	env  []string
	name string
	args []string
	cmd  *exec.Cmd
}

func helper(rec *recorded, script map[string]string) func(dir string, env []string, name string, args ...string) *exec.Cmd {
	return func(dir string, env []string, name string, args ...string) *exec.Cmd {
		if rec != nil {
			rec.dir, rec.env, rec.name, rec.args = dir, slices.Clone(env), name, slices.Clone(args)
		}
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // re-invokes the test binary
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FUZZCAMPAIGN_HELPER=1")
		for k, v := range script {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if rec != nil {
			rec.cmd = cmd
		}
		return cmd
	}
}

func testEnv(t *testing.T, script map[string]string, rec *recorded) Env {
	t.Helper()
	root := t.TempDir()
	env := Env{
		Work:     filepath.Join(root, "work"),
		Out:      filepath.Join(root, "out"),
		LogCap:   1 << 20,
		Parallel: 2,
		BaseEnv:  []string{"A=1"},
		CacheDir: func(Target) string { return filepath.Join(root, "cache", "fuzz", "x", "y", "z") },
		Command:  helper(rec, script),
		Limit:    func(Item) time.Duration { return 60 * time.Second },
	}
	env.CorpusMax.Files, env.CorpusMax.Bytes = 2000, 64<<20
	for _, d := range []string{env.Work, env.Out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

var testItem = Item{Target: Target{"internal/x", "FuzzX"}, Fuzztime: 2 * time.Minute, Minimize: 10 * time.Second}

func progressFile(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "progress", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerBuildsTheDocumentedCommand(t *testing.T) {
	var rec recorded
	env := testEnv(t, map[string]string{"HELPER_PRINT": "PASS"}, &rec)
	if _, err := (GoRunner{}).Run(context.Background(), testItem, env); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"test", "-run=^$", "-fuzz=^FuzzX$", "-fuzztime=2m0s", "-parallel=2",
		"-fuzzminimizetime=10s", "-timeout=12m10s", "./internal/x",
	}
	if rec.name != "go" || !slices.Equal(rec.args, wantArgs) {
		t.Errorf("command %s %q, want go %q", rec.name, rec.args, wantArgs)
	}
	if rec.dir != env.Work {
		t.Errorf("dir %q, want the work copy %q", rec.dir, env.Work)
	}
	if want := []string{"A=1", "CGO_ENABLED=0", "GOFLAGS=-p=2"}; !slices.Equal(rec.env, want) {
		t.Errorf("env %q, want exactly %q", rec.env, want)
	}
}

func TestRunnerParsesProgressAndClassifies(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		exit      string
		want      Outcome
		execs     uint64
		newInter  uint64
		wantNoteS string
	}{
		{"ok", "fuzzing.txt", "0", OK, 79918, 43, ""},
		{"stalled", "stall.txt", "0", Stalled, 335459, 0, "stall"},
		{"no progress", "baseline.txt", "0", NoProgress, 0, 0, "execution"},
		{"failed", "failed.txt", "1", Failed, 75148, 7, "failing"},
		{"build failed", "build-failed.txt", "1", BuildFailed, 0, 0, "build"},
	}
	for _, c := range cases {
		env := testEnv(t, map[string]string{"HELPER_CAT": progressFile(t, c.file), "HELPER_EXIT": c.exit}, nil)
		res, err := (GoRunner{}).Run(context.Background(), testItem, env)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if res.Outcome != c.want || res.Execs != c.execs || res.NewInteresting != c.newInter {
			t.Errorf("%s: outcome %q execs %d new %d; want %q %d %d", c.name, res.Outcome, res.Execs, res.NewInteresting, c.want, c.execs, c.newInter)
		}
		if c.want != OK && !strings.Contains(strings.ToLower(res.Note), c.wantNoteS) {
			t.Errorf("%s: note %q lacks %q", c.name, res.Note, c.wantNoteS)
		}
		if c.want == OK && res.Note != "" {
			t.Errorf("%s: an ok result has a note %q", c.name, res.Note)
		}
		if res.Target != testItem.Target {
			t.Errorf("%s: target %v", c.name, res.Target)
		}
	}
}

func TestRunnerCapsTheLogAndCountsTheCut(t *testing.T) {
	env := testEnv(t, map[string]string{"HELPER_LINES": "100000"}, nil)
	env.LogCap = 4096
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	marker := "[log truncated: " + strconv.Itoa(res.LogDroppedLines) + " lines dropped]\n"
	if !bytes.HasSuffix(data, []byte(marker)) {
		t.Fatalf("the log does not end with %q", marker)
	}
	body := strings.TrimSuffix(string(data), marker)
	if int64(len(body)) > env.LogCap {
		t.Errorf("log body %d bytes, cap %d", len(body), env.LogCap)
	}
	kept := strings.Count(body, "\n")
	if kept+res.LogDroppedLines != 100000 || res.LogDroppedLines == 0 {
		t.Errorf("kept %d + dropped %d != 100000", kept, res.LogDroppedLines)
	}
	if res.Execs != 100000 {
		t.Errorf("Execs = %d: progress parsing missed the dropped lines", res.Execs)
	}
}

func TestRunnerDrainsAnOverlongLine(t *testing.T) {
	env := testEnv(t, map[string]string{"HELPER_BIGLINE": strconv.Itoa(10 << 20)}, nil)
	done := make(chan RunResult, 1)
	go func() {
		res, _ := (GoRunner{}).Run(context.Background(), testItem, env)
		done <- res
	}()
	select {
	case res := <-done:
		if res.Execs != 7 {
			t.Errorf("Execs = %d, want 7 from the line after the overlong one", res.Execs)
		}
		if !strings.Contains(res.Note, "overlong") {
			t.Errorf("note %q does not mention the overlong line", res.Note)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the runner hangs on a 10 MiB line")
	}
}

func TestFindingsAreCopiedByteForByteOutOfTheCopy(t *testing.T) {
	srcRoot := t.TempDir()
	env := testEnv(t, nil, nil)
	input := filepath.Join(env.Work, "internal", "x", "testdata", "fuzz", "FuzzX", "abc")
	env.Command = helper(nil, map[string]string{
		"HELPER_PRINT":  "    Failing input written to testdata/fuzz/FuzzX/abc",
		"HELPER_CREATE": input,
		"HELPER_EXIT":   "1",
	})
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Failed || len(res.Findings) != 1 {
		t.Fatalf("outcome %q findings %v; want failed with one finding", res.Outcome, res.Findings)
	}
	want := filepath.Join(env.Out, "findings", "internal", "x", "FuzzX", "abc")
	if res.Findings[0] != want {
		t.Errorf("finding at %q, want %q", res.Findings[0], want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(input)
	if !bytes.Equal(got, orig) || len(got) == 0 {
		t.Errorf("the copy differs from the original: %x vs %x", got, orig)
	}
	readme, err := os.ReadFile(filepath.Join(filepath.Dir(want), "README.txt"))
	if err != nil || !strings.Contains(string(readme), "-run=FuzzX/abc") {
		t.Errorf("README.txt missing the re-run command: %q, %v", readme, err)
	}
	if es, _ := os.ReadDir(srcRoot); len(es) != 0 {
		t.Errorf("something was created in the source root: %v", es)
	}
}

func TestFindingOutsideTheCopyIsNotCopied(t *testing.T) {
	env := testEnv(t, map[string]string{
		"HELPER_PRINT": "Failing input written to ../../../../etc/passwd",
		"HELPER_EXIT":  "1",
	}, nil)
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 || !strings.Contains(res.Note, "outside") {
		t.Errorf("findings %v note %q; want none and a note about a path outside the copy", res.Findings, res.Note)
	}
}

func TestCorpusIsPrunedAfterEachTarget(t *testing.T) {
	env := testEnv(t, map[string]string{"HELPER_CAT": progressFile(t, "fuzzing.txt")}, nil)
	dir := env.CacheDir(testItem.Target)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		corpusFile(t, dir, "e"+strconv.Itoa(i), 1, corpusBase.Add(time.Duration(i)*time.Second))
	}
	env.CorpusMax.Files = 2
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, dir); !slices.Equal(got, []string{"e3", "e4"}) {
		t.Errorf("corpus left %v, want the two newest", got)
	}
	if res.Outcome != OK {
		t.Errorf("outcome %q (note %q)", res.Outcome, res.Note)
	}
}

func TestCorpusPruneFailureIsANoteNotAPass(t *testing.T) {
	env := testEnv(t, map[string]string{"HELPER_CAT": progressFile(t, "fuzzing.txt")}, nil)
	env.CacheDir = func(Target) string { return "relative/unsafe" }
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Note == "" || res.Outcome == OK {
		t.Errorf("outcome %q note %q; a prune failure must not read as a clean pass", res.Outcome, res.Note)
	}
}

func TestRunnerReportsATargetThatCannotStart(t *testing.T) {
	env := testEnv(t, nil, nil)
	env.Command = func(dir string, _ []string, _ string, _ ...string) *exec.Cmd {
		return exec.Command(filepath.Join(dir, "no-such-binary")) //nolint:gosec // a command that cannot start
	}
	if _, err := (GoRunner{}).Run(context.Background(), testItem, env); err == nil {
		t.Errorf("a command that cannot start was not an error")
	}
}
