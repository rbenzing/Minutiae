// Command check runs every verification step that must pass before work is
// called done: module tidiness, vet, lint (incl. formatting), build and tests.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	steps := [][]string{
		{"go", "mod", "tidy", "-diff"},
		{"go", "vet", "./..."},
		{"go", "tool", "golangci-lint", "run", "./..."},
		{"go", "build", "-o", binPath(), "./cmd/minutiae"},
		testCmd(),
	}
	for _, s := range steps {
		fmt.Printf("==> %s\n", strings.Join(s, " "))
		cmd := exec.Command(s[0], s[1:]...) //nolint:gosec // fixed, trusted command list
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "CHECK FAILED at %q: %v\n", strings.Join(s, " "), err)
			os.Exit(1)
		}
	}
	fmt.Println("CHECK PASSED")
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
		return []string{"go", "test", "-race", "-count=1", "./..."}
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
