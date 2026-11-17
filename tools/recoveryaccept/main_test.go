package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot is the repository root, found by walking up from the package directory to go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("no go.mod above the package directory")
		}
		d = p
	}
}

// fakeModule is a directory that passes the module check.
func fakeModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module "+modulePath+"\n\ngo 1.26\n")
	return root
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every refusal is a usage error, exit 2, before anything is written.
func TestScratchRefusals(t *testing.T) {
	root := fakeModule(t)
	outside := t.TempDir()
	notFile := filepath.Join(root, "adir")
	if err := os.Mkdir(notFile, 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	write(t, filepath.Join(foreign, "go.mod"), "module example.com/other\n")
	cases := []struct {
		name string
		cwd  string
		args []string
		want string
	}{
		{"dir outside the project", root, []string{"-dir", outside}, "inside the project"},
		{"dir under cases", root, []string{"-dir", filepath.Join(root, "cases", "x")}, "cases"},
		{"dir under Cases", root, []string{"-dir", filepath.Join(root, "work", "Cases")}, "cases"},
		{"dir above the project", root, []string{"-dir", filepath.Dir(root)}, "inside the project"},
		{"no go.mod", t.TempDir(), nil, "go.mod"},
		{"foreign module", foreign, nil, "go.mod"},
		{"bin is a directory", root, []string{"-bin", notFile}, "regular file"},
		{"bin is missing", root, []string{"-bin", filepath.Join(root, "nothing")}, "regular file"},
		{"unknown flag", root, []string{"-nope"}, "nope"},
		{"stray argument", root, []string{"extra"}, "extra"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := listing(t, c.cwd)
			if _, err := parseFlags(c.args, c.cwd); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("parseFlags: %v, want a usage error naming %q", err, c.want)
			} else if _, ok := err.(usageError); !ok {
				t.Errorf("parseFlags: %T, want a usageError", err)
			}
			var out, errOut bytes.Buffer
			if code := run(context.Background(), c.args, c.cwd, &fakeRunner{}, &out, &errOut); code != exitUsage {
				t.Errorf("run: exit %d, want 2\n%s", code, errOut.String())
			}
			if out.Len() != 0 {
				t.Errorf("run printed to stdout before refusing: %q", out.String())
			}
			if after := listing(t, c.cwd); strings.Join(before, "|") != strings.Join(after, "|") {
				t.Errorf("a refusal wrote to the working directory: %v -> %v", before, after)
			}
		})
	}
	t.Run("a good invocation", func(t *testing.T) {
		o, err := parseFlags([]string{"-keep", "-verbose"}, root)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(root, ".claude", "scratch"); o.dir != want || !o.keep || !o.verbose || o.root != root {
			t.Errorf("options %+v, want dir %s", o, want)
		}
		if o, err := parseFlags([]string{"-dir", "work/out"}, root); err != nil || o.dir != filepath.Join(root, "work", "out") {
			t.Errorf("a relative -dir: %+v %v", o, err)
		}
	})
}

func TestBinaryNameFollowsGOOS(t *testing.T) {
	if got := binaryName("windows"); got != "minutiae.exe" {
		t.Errorf("windows: %q", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := binaryName(goos); got != "minutiae" {
			t.Errorf("%s: %q", goos, got)
		}
	}
}
