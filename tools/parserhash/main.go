// Command parserhash maintains internal/parsers/parsers.lock and
// identity_gen.go: the source hash of every compiled-in parser, locked by
// version. "-check" fails when a parser's source changed without a version
// bump, a version is not locked, or a generated file is stale; "-write"
// appends new versions (never rewrites a locked line) and regenerates
// identity_gen.go.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rbenzing/minutiae/internal/parsers"
)

// Entry is one compiled-in parser as the registry reports it.
type Entry struct{ Name, Version, Package string }

// Problem is one reason -check fails.
type Problem struct{ Parser, Message string }

const (
	lockRel = "internal/parsers/parsers.lock"
	genRel  = "internal/parsers/identity_gen.go"
)

func entriesFromRegistry() []Entry {
	var out []Entry
	for _, p := range parsers.All() {
		m := p.Meta()
		out = append(out, Entry{Name: m.Name, Version: m.Version, Package: parsers.PackagePath(p)})
	}
	return out
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, entriesFromRegistry()))
}

// hashed is an entry with the hash of its scope.
type hashed struct {
	Entry
	Hash string
}

func hashEntries(cfg Config, entries []Entry) ([]hashed, []Problem, error) {
	var out []hashed
	var probs []Problem
	for _, e := range entries {
		who := e.Name + "@" + e.Version
		if !strings.HasPrefix(e.Package, cfg.ModulePath+"/internal/parsers/") {
			probs = append(probs, Problem{who, fmt.Sprintf("package %q is outside %s/internal/parsers", e.Package, cfg.ModulePath)})
			continue
		}
		s, err := ResolveScope(cfg, e.Package)
		if err != nil {
			return nil, nil, err
		}
		h, err := HashScope(s, func(dir, file string) ([]byte, error) {
			return readFile(filepath.Join(dir, filepath.FromSlash(file)))
		})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, hashed{e, h})
	}
	return out, probs, nil
}

// judge compares one entry with the lock: "" when it is locked with this hash.
func judge(h hashed, lock Lock) (msg string, locked bool) {
	if got, ok := lock.find(h.Name, h.Version); ok {
		if got == h.Hash {
			return "", true
		}
		return fmt.Sprintf("source changed since version %s was locked: bump Meta.Version (a formatter rewrite that changes the syntax tree, such as 0777 to 0o777, counts as a source change)", h.Version), true
	}
	if top, ok := lock.highest(h.Name); ok && compareVersions(h.Version, top) < 0 {
		return fmt.Sprintf("version %s is below the highest locked version %s (a reverted version)", h.Version, top), true
	}
	return fmt.Sprintf("version %s is not locked (run go run ./tools/parserhash -write)", h.Version), false
}

func identities(hs []hashed) map[string]GenIdentity {
	ids := map[string]GenIdentity{}
	for _, h := range hs {
		ids[h.Name+"@"+h.Version] = GenIdentity{Package: h.Package, Hash: h.Hash}
	}
	return ids
}

// Check compares the registry with the lock and the generated identities.
func Check(cfg Config, entries []Entry, lock Lock, gen []byte) ([]Problem, error) {
	hs, probs, err := hashEntries(cfg, entries)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if msg, _ := judge(h, lock); msg != "" {
			probs = append(probs, Problem{h.Name + "@" + h.Version, msg})
		}
	}
	if !bytes.Equal(gen, GenerateIdentity(identities(hs))) {
		probs = append(probs, Problem{"identity_gen.go", "is missing or stale (run go run ./tools/parserhash -write)"})
	}
	return probs, nil
}

// Write appends the entries not yet locked and regenerates identity_gen.go.
// It never rewrites a locked line: a changed hash or a reverted version is an
// error with the text Check reports.
func Write(cfg Config, entries []Entry, lock Lock) (Lock, []byte, []LockLine, error) {
	hs, probs, err := hashEntries(cfg, entries)
	if err != nil {
		return lock, nil, nil, err
	}
	if len(probs) != 0 {
		return lock, nil, nil, fmt.Errorf("%s: %s", probs[0].Parser, probs[0].Message)
	}
	next := Lock{Header: lock.Header, Lines: append([]LockLine(nil), lock.Lines...)}
	var added []LockLine
	for _, h := range hs {
		msg, locked := judge(h, lock)
		switch {
		case msg != "":
			if locked {
				return lock, nil, nil, fmt.Errorf("%s@%s: %s", h.Name, h.Version, msg)
			}
			ln := LockLine{h.Name, h.Version, h.Hash}
			next.Lines = append(next.Lines, ln)
			added = append(added, ln)
		}
	}
	next.sorted()
	return next, GenerateIdentity(identities(hs)), added, nil
}

func modulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`), nil
		}
	}
	return "", errors.New("no module line in go.mod")
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// run implements the command: -check or -write; exit 0, 1 (problems or
// failure) or 2 (usage).
func run(args []string, stdout, stderr io.Writer, entries []Entry) int {
	fl := flag.NewFlagSet("parserhash", flag.ContinueOnError)
	fl.SetOutput(stderr)
	check := fl.Bool("check", false, "verify the lock and identity_gen.go")
	write := fl.Bool("write", false, "append new versions to the lock and regenerate identity_gen.go")
	root := fl.String("root", ".", "module root")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 || *check == *write {
		fmt.Fprintln(stderr, "usage: parserhash [-root dir] -check | -write")
		return 2
	}
	mp, err := modulePath(*root)
	if err != nil {
		fmt.Fprintf(stderr, "parserhash: %v\n", err)
		return 1
	}
	cfg := Config{ModuleRoot: *root, ModulePath: mp}
	lockPath, genPath := filepath.Join(*root, filepath.FromSlash(lockRel)), filepath.Join(*root, filepath.FromSlash(genRel))
	lockBytes, err := readOptional(lockPath)
	if err != nil {
		fmt.Fprintf(stderr, "parserhash: %v\n", err)
		return 1
	}
	lock, err := ParseLock(lockBytes)
	if err != nil {
		fmt.Fprintf(stderr, "parserhash: %s: %v\n", lockRel, err)
		return 1
	}
	genBytes, err := readOptional(genPath)
	if err != nil {
		fmt.Fprintf(stderr, "parserhash: %v\n", err)
		return 1
	}
	if *check {
		probs, err := Check(cfg, entries, lock, genBytes)
		if err != nil {
			fmt.Fprintf(stderr, "parserhash: %v\n", err)
			return 1
		}
		for _, p := range probs {
			fmt.Fprintf(stderr, "parserhash: %s: %s\n", p.Parser, p.Message)
		}
		if len(probs) != 0 {
			return 1
		}
		return 0
	}
	newLock, gen, added, err := Write(cfg, entries, lock)
	if err != nil {
		fmt.Fprintf(stderr, "parserhash: %v\n", err)
		return 1
	}
	if len(newLock.Header) == 0 {
		newLock.Header = []string{"# parsers.lock: the generated source hash of every compiled-in parser, locked by version."}
	}
	for path, data := range map[string][]byte{lockPath: newLock.Format(), genPath: gen} {
		if old, _ := readOptional(path); bytes.Equal(old, data) {
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // source files, world-readable
			fmt.Fprintf(stderr, "parserhash: %v\n", err)
			return 1
		}
	}
	for _, ln := range added {
		fmt.Fprintf(stdout, "locked %s %s %s\n", ln.Name, ln.Version, ln.Hash)
	}
	if len(added) == 0 {
		fmt.Fprintln(stdout, "nothing to add")
	}
	return 0
}

// readFile reads the files being hashed; a variable so a test can make the
// read fail after the scope was resolved.
var readFile = os.ReadFile
