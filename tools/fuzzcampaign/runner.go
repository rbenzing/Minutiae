package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// nowFunc is the campaign clock; a variable so a test can move it.
var nowFunc = time.Now

// Env is the environment of a campaign.
type Env struct {
	Work      string   // the throwaway source copy the fuzzers run in
	Out       string   // campaign output directory
	LogCap    int64    // bytes per target log, default 8 MiB
	Parallel  int      // fuzz workers per target (go test -parallel)
	CorpusMax struct { // per target: the fuzz corpus cache is pruned to these caps
		Files int
		Bytes int64
	}
	OnResult func([]RunResult)                                                     // called with all results so far after each target; nil means none
	BaseEnv  []string                                                              // environment of the go test process; nil means os.Environ()
	CacheDir func(Target) string                                                   // the target's fuzz corpus cache directory; nil means <GOCACHE>/fuzz/<module>/<pkg>/<Name>
	Command  func(dir string, env []string, name string, args ...string) *exec.Cmd // nil means exec.Command with Dir and Env set
	Limit    func(Item) time.Duration                                              // the context deadline of one target; nil means FuzzLimit
}

// RunResult is the result of one target.
type RunResult struct {
	Target                Target
	Outcome               Outcome
	Execs, NewInteresting uint64
	Elapsed               time.Duration
	Findings              []string // paths under Out/findings, never inside the repository
	LogPath               string
	LogDroppedLines       int    // lines past LogCap: counted, still parsed for progress
	Note                  string // why, for every outcome but ok
}

// Runner runs one planned target.
type Runner interface {
	Run(ctx context.Context, it Item, env Env) (RunResult, error)
}

const (
	maxLineKept   = 4096
	defaultLogCap = 8 << 20
	graceTerm     = 5 * time.Second
	pipeDrain     = 5 * time.Second
)

// FuzzArgs is the go test argument vector for one item.
func FuzzArgs(it Item, parallel int) []string {
	return []string{
		"test", "-run=^$", "-fuzz=^" + it.Name + "$",
		"-fuzztime=" + it.Fuzztime.String(), "-parallel=" + strconv.Itoa(parallel),
		"-fuzzminimizetime=" + it.Minimize.String(),
		"-timeout=" + (it.Fuzztime + it.Minimize + 10*time.Minute).String(),
		"./" + it.Pkg,
	}
}

// FuzzEnv is the environment of the go test process: base plus the two documented variables.
func FuzzEnv(base []string) []string {
	return append(append([]string(nil), base...), "CGO_ENABLED=0", "GOFLAGS=-p=2")
}

// FuzzLimit is the context deadline of one target: one minute longer than the go test
// timeout, so a go-level timeout panic classifies as failed with its log, not as a timeout.
func FuzzLimit(it Item) time.Duration { return it.Fuzztime + it.Minimize + 11*time.Minute }

func defaultCommand(dir string, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed argument vector built from validated names
	cmd.Dir, cmd.Env = dir, env
	return cmd
}

// GoRunner runs `go test -fuzz` as a child process in its own process group.
type GoRunner struct{}

// Run runs one target. A command that cannot start is an error; every other result,
// including failures and timeouts, is an outcome.
func (GoRunner) Run(ctx context.Context, it Item, env Env) (RunResult, error) {
	res := RunResult{Target: it.Target}
	started := nowFunc()
	base := env.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	command := env.Command
	if command == nil {
		command = defaultCommand
	}
	cmd := command(env.Work, FuzzEnv(base), "go", FuzzArgs(it, env.Parallel)...)
	setGroup(cmd)
	pr, pw, err := os.Pipe()
	if err != nil {
		return res, err
	}
	defer func() { _ = pr.Close() }()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return res, fmt.Errorf("start %s: %w", it.Name, err)
	}
	_ = pw.Close()

	limit := FuzzLimit(it)
	if env.Limit != nil {
		limit = env.Limit(it)
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	stopped := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-runCtx.Done():
			stopProcess(cmd.Process, graceTerm)
		case <-stopped:
		}
	}()

	logPath := filepath.Join(env.Out, "logs", filepath.FromSlash(it.Pkg), it.Name+".log")
	lg, lerr := newLogWriter(logPath, env.LogCap)
	if lerr != nil {
		stopProcess(cmd.Process, 0)
		_ = cmd.Wait()
		close(stopped)
		<-watched
		return res, lerr
	}
	var obs Observation
	var last Progress
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		br := bufio.NewReaderSize(pr, maxLineKept+1)
		for {
			line, overlong, rerr := readLine(br)
			if line != nil || overlong || rerr == nil {
				text := string(line)
				obs.AddLine(text)
				if p, ok := ParseProgress(strings.TrimSuffix(text, "\r")); ok {
					last = p
				}
				lg.line(text, overlong)
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	select {
	case <-readerDone:
	case <-time.After(pipeDrain):
		_ = pr.Close() // a descendant kept the pipe open past the process
		<-readerDone
	}
	close(stopped)
	<-watched
	res.LogPath, res.LogDroppedLines = lg.close()

	obs.Exit = exitCode(waitErr)
	obs.TimedOut = runCtx.Err() != nil
	res.Outcome = Classify(obs)
	res.Execs, res.NewInteresting = last.Execs, last.NewInteresting
	res.Elapsed = nowFunc().Sub(started)
	notes := outcomeNote(res.Outcome, obs, runCtx.Err())
	if obs.Overlong > 0 {
		notes = append(notes, fmt.Sprintf("%d overlong line(s) over %d bytes were cut", obs.Overlong, maxLineKept))
	}
	if res.LogDroppedLines > 0 {
		notes = append(notes, fmt.Sprintf("log truncated: %d lines dropped", res.LogDroppedLines))
	}
	if obs.FailingTotal > len(obs.Failing) {
		notes = append(notes, fmt.Sprintf("%d failing inputs, %d named", obs.FailingTotal, len(obs.Failing)))
	}

	findings, fnotes := copyFindings(env, it, obs.Failing)
	res.Findings = findings
	notes = append(notes, fnotes...)

	if pnote := pruneCorpus(env, it.Target, res.NewInteresting); pnote != "" {
		notes = append(notes, pnote)
		if res.Outcome == OK {
			res.Outcome = Failed
		}
	}
	res.Note = strings.Join(notes, "; ")
	return res, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if c := ee.ExitCode(); c != 0 {
			return c
		}
		return -1
	}
	return -1
}

func outcomeNote(o Outcome, obs Observation, ctxErr error) []string {
	switch o {
	case Failed:
		if obs.FailingTotal > 0 {
			return []string{"failing input written (" + strconv.Itoa(obs.FailingTotal) + ")"}
		}
		return []string{"go test exited with status " + strconv.Itoa(obs.Exit)}
	case Stalled:
		return []string{"stall: the engine's execution counter stopped moving while it still reported fuzzing"}
	case NoProgress:
		return []string{"no execution reported by the engine"}
	case Timeout:
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return []string{"timeout: the process group was stopped at the deadline"}
		}
		return []string{"cancelled: the process group was stopped"}
	case BuildFailed:
		return []string{"build failed"}
	}
	return nil
}

// readLine returns the next line without its newline. A line longer than maxLineKept is
// returned cut to maxLineKept+1 bytes with overlong set, and the rest of it is drained, so the
// child can never block on a full pipe.
func readLine(br *bufio.Reader) (line []byte, overlong bool, err error) {
	chunk, err := br.ReadSlice('\n')
	switch {
	case err == nil:
		return append([]byte(nil), chunk[:len(chunk)-1]...), false, nil
	case errors.Is(err, bufio.ErrBufferFull):
		kept := append([]byte(nil), chunk...)
		for {
			_, derr := br.ReadSlice('\n')
			if derr == nil {
				return kept, true, nil
			}
			if !errors.Is(derr, bufio.ErrBufferFull) {
				return kept, true, derr
			}
		}
	default: // EOF or a read error: whatever is left is a final unterminated line
		if len(chunk) == 0 {
			return nil, false, err
		}
		return append([]byte(nil), chunk...), false, err
	}
}

// logWriter writes lines to a log file until the cap, then counts the rest.
type logWriter struct {
	f       *os.File
	path    string
	cap     int64
	written int64
	dropped int
}

func newLogWriter(p string, capBytes int64) (*logWriter, error) {
	if capBytes <= 0 {
		capBytes = defaultLogCap
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &logWriter{f: f, path: p, cap: capBytes}, nil
}

func (l *logWriter) line(text string, overlong bool) {
	if overlong {
		text = text[:min(len(text), maxLineKept)] + " [line cut]"
	}
	n := int64(len(text)) + 1
	if l.written+n > l.cap {
		l.dropped++
		return
	}
	if _, err := l.f.WriteString(text + "\n"); err != nil {
		l.dropped++
		return
	}
	l.written += n
}

func (l *logWriter) close() (string, int) {
	if l.dropped > 0 {
		_, _ = l.f.WriteString("[log truncated: " + strconv.Itoa(l.dropped) + " lines dropped]\n")
	}
	_ = l.f.Close()
	return l.path, l.dropped
}

// copyFindings copies the failing inputs the run named out of the work copy into
// Out/findings/<pkg>/<Name>/, byte for byte, with the re-run command next to them.
func copyFindings(env Env, it Item, failing []string) (copied, notes []string) {
	for _, raw := range failing {
		src := filepath.FromSlash(strings.ReplaceAll(raw, `\`, "/"))
		if !filepath.IsAbs(src) {
			src = filepath.Join(env.Work, filepath.FromSlash(it.Pkg), src)
		}
		src = filepath.Clean(src)
		rel, err := filepath.Rel(env.Work, src)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			notes = append(notes, fmt.Sprintf("finding path %q is outside the work copy and was not copied", raw))
			continue
		}
		dstDir := filepath.Join(env.Out, "findings", filepath.FromSlash(it.Pkg), it.Name)
		dst := filepath.Join(dstDir, filepath.Base(src))
		if err := copyFileExact(src, dst); err != nil {
			notes = append(notes, fmt.Sprintf("finding %q not copied: %v", raw, err))
			continue
		}
		readme := fmt.Sprintf("Failing input of %s.%s, copied byte for byte from the work copy (untrusted data: do not open it with a tool that interprets it).\n"+
			"Re-run: copy the file to <package dir>/testdata/fuzz/%s/ and run\n  go test -run=%s/%s ./%s\n",
			it.Pkg, it.Name, it.Name, it.Name, filepath.Base(src), it.Pkg)
		if err := os.WriteFile(filepath.Join(dstDir, "README.txt"), []byte(readme), 0o600); err != nil {
			notes = append(notes, "README.txt not written: "+err.Error())
		}
		copied = append(copied, dst)
	}
	return copied, notes
}

func copyFileExact(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// pruneCorpus prunes the target's fuzz corpus cache to the caps and returns a note for a
// failure ("" when it worked or there was nothing to prune).
func pruneCorpus(env Env, t Target, newInteresting uint64) string {
	dir := ""
	if env.CacheDir != nil {
		dir = env.CacheDir(t)
	} else {
		dir = defaultCacheDir(env.Work, env.BaseEnv, t)
	}
	if dir == "" {
		return "corpus cache directory unknown: not pruned"
	}
	files, bytes := env.CorpusMax.Files, env.CorpusMax.Bytes
	if files == 0 {
		files = 2000
	}
	if bytes == 0 {
		bytes = 64 << 20
	}
	_, err := PruneCorpus(dir, files, bytes)
	var skipped *SkippedError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if newInteresting > 0 {
			// The engine reported new interesting inputs, so it wrote a cache directory: a
			// missing one means this path is wrong and the cap was not enforced.
			return "corpus cache directory " + dir + " is missing although the target reported new interesting inputs: not pruned"
		}
		return ""
	case err == nil, errors.As(err, &skipped):
		return ""
	}
	return "corpus not pruned: " + err.Error()
}

// defaultCacheDir is <GOCACHE>/fuzz/<module path>/<pkg>/<Name>, the place `go test -fuzz`
// keeps the corpus it grows.
func defaultCacheDir(work string, base []string, t Target) string {
	if base == nil {
		base = os.Environ()
	}
	out, err := defaultCommand(work, base, "go", "env", "GOCACHE").Output()
	if err != nil {
		return ""
	}
	mod, err := os.ReadFile(filepath.Join(work, "go.mod"))
	if err != nil {
		return ""
	}
	module := ""
	for _, l := range strings.Split(string(mod), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			module = strings.TrimSpace(m)
			break
		}
	}
	if module == "" {
		return ""
	}
	imp := module
	if t.Pkg != "." {
		imp = path.Join(module, t.Pkg)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "fuzz", filepath.FromSlash(imp), t.Name)
}

// Campaign runs the plan's targets in order. A target is attempted only while it can finish
// before the deadline; the rest are returned as Skipped. The exit code is 0 only when every
// target is ok.
func Campaign(ctx context.Context, p Plan, r Runner, env Env, deadline time.Time) ([]RunResult, int) {
	results := make([]RunResult, 0, len(p.Items))
	code := 0
	for _, it := range p.Items {
		var res RunResult
		if ctx.Err() != nil {
			res = RunResult{Target: it.Target, Outcome: Skipped, Note: "cancelled: the campaign was interrupted before this target started"}
		} else if need := it.Fuzztime + it.Minimize + 11*time.Minute; nowFunc().Add(need).After(deadline) {
			res = RunResult{Target: it.Target, Outcome: Skipped, Note: "campaign deadline"}
		} else if rr, err := r.Run(ctx, it, env); err != nil {
			res = RunResult{Target: it.Target, Outcome: Failed, Note: err.Error()}
		} else {
			res = rr
		}
		if res.Outcome != OK {
			code = 1
		}
		results = append(results, res)
		if env.OnResult != nil {
			env.OnResult(results)
		}
	}
	return results, code
}
