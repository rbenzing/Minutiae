// Command check runs every verification step that must pass before work is
// called done: module tidiness, vet, lint (incl. formatting), build, the
// CGO_ENABLED=0 host build and cross-builds, and tests.
package main

import (
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
		{args: testCmd()},
	}
	for _, s := range steps {
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

func testCmd() []string {
	if raceSupported() {
		return []string{"go", "test", "-race", "-timeout", "30m", "-count=1", "./..."}
	}
	fmt.Println("note: race detector unavailable (needs cgo and a C compiler); running tests without -race")
	return []string{"go", "test", "-count=1", "./..."}
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
