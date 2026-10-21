package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DockerOptions describes one container run.
type DockerOptions struct {
	Image       string   // golang:<toolchain>@sha256:<digest> (or minutiae-fuzz:1 built from the Dockerfile)
	Name        string   // minutiae-fuzz-<unix seconds>-<pid>; built from numbers only
	SrcCopy     string   // absolute host path of the host-made copy
	LinuxBinary string   // absolute host path of the linux/amd64 build of this tool
	Out         string   // absolute host path of the output directory
	Args        []string // the inner `fuzzcampaign run -mode host ...` arguments
}

// Cache locations inside the container: under the one read-write bind (the output directory),
// so nothing large is written into the container or a Docker volume.
const (
	containerModCache = "/out/.cache/gomod"
	containerBuild    = "/out/.cache/go-build"
)

// DockerArgs is the full `docker run` argument list (without the leading "docker"): bounded
// and unprivileged, no network, the source and the tool read-only, the output directory the
// only read-write bind.
func DockerArgs(o DockerOptions) []string {
	a := []string{
		"run", "--rm", "--name", o.Name,
		"--memory", "4g", "--memory-swap", "4g", "--cpus", "2", "--pids-limit", "1024",
		"--network", "none",
		"-e", "GOPROXY=off", "-e", "GOTOOLCHAIN=local", "-e", "CGO_ENABLED=0",
		"-e", "GOMODCACHE=" + containerModCache, "-e", "GOCACHE=" + containerBuild,
		"-v", o.SrcCopy + ":/src:ro",
		"-v", o.LinuxBinary + ":/usr/local/bin/fuzzcampaign:ro",
		"-v", o.Out + ":/out",
		o.Image,
		"fuzzcampaign", "run", "-mode", "host", "-src", "/src", "-out", "/out",
	}
	return append(a, o.Args...)
}

// DockerPrepareArgs is the argument list of the prepare run: the same image, the copy
// read-only and the output directory for the module cache, but with the network (the only
// step that has it). It downloads exactly the modules the given packages compile and nothing
// else: the template skips the main module, whose empty version would make `go mod download`
// fail, and `xargs -r` runs nothing when the list is empty (a bare `go mod download` would
// fetch every requirement, including those of the pinned lint tool).
func DockerPrepareArgs(o DockerOptions, pkgs []string) []string {
	script := "go list -deps -test -f '{{if and .Module (not .Module.Main)}}{{.Module.Path}}@{{.Module.Version}}{{end}}' " +
		strings.Join(pkgs, " ") + " | sort -u | xargs -r go mod download"
	return []string{
		"run", "--rm", "--name", o.Name,
		"-e", "GOTOOLCHAIN=local", "-e", "CGO_ENABLED=0", "-e", "GOFLAGS=-modcacherw",
		"-e", "GOMODCACHE=" + containerModCache, "-e", "GOCACHE=" + containerBuild,
		"-v", o.SrcCopy + ":/src:ro",
		"-v", o.Out + ":/out",
		"-w", "/src",
		o.Image,
		"sh", "-c", script,
	}
}

// DockerKillArgs is `docker kill <name>` (without the leading "docker").
func DockerKillArgs(name string) []string { return []string{"kill", name} }

// DockerRemoveArgs is `docker rm -f <name>` (without the leading "docker").
func DockerRemoveArgs(name string) []string { return []string{"rm", "-f", name} }

var (
	goVersionRE = regexp.MustCompile(`^go version go([0-9][0-9.]*) linux/amd64$`)
	toolchainRE = regexp.MustCompile(`(?m)^toolchain go([0-9][0-9.]*)\s*$`)
)

// CheckImageGoVersion compares the output of `go version` inside the image with go.mod's
// toolchain line: a linux/amd64 Go of exactly that version, or an error.
func CheckImageGoVersion(output, goMod string) error {
	m := goVersionRE.FindStringSubmatch(strings.TrimSpace(output))
	if m == nil {
		return fmt.Errorf("fuzzcampaign: cannot read a linux/amd64 Go version from %q", output)
	}
	t := toolchainRE.FindStringSubmatch(goMod)
	if t == nil {
		return errors.New("fuzzcampaign: go.mod has no toolchain line to compare the image with")
	}
	if m[1] != t[1] {
		return fmt.Errorf("fuzzcampaign: the image has Go %s but go.mod's toolchain is %s: bump the digest in the Dockerfile", m[1], t[1])
	}
	return nil
}

// DockerName builds a container name from numbers only.
func DockerName(now time.Time, pid int) string {
	return fmt.Sprintf("minutiae-fuzz-%d-%d", now.Unix(), pid)
}

// Execer runs an external command and returns its exit code; an error is a failure to run it
// (or a context that ended), not a non-zero exit.
type Execer interface {
	Run(ctx context.Context, name string, args ...string) (int, error)
}

// osExec is the real Execer: it passes the command's output through.
type osExec struct{}

func (osExec) Run(ctx context.Context, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed argument vector built from validated names
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee) && ctx.Err() == nil:
		return ee.ExitCode(), nil
	}
	return -1, err
}

// RunContainer runs the container under a context of at most limit and, on every way out
// (normal exit, deadline, cancellation, an early error), runs `docker kill <name>` and then
// `docker rm -f <name>`, by the recorded name only and once. It returns the tool's exit code
// and the cause when it is not the inner tool's own code.
func RunContainer(ctx context.Context, ex Execer, o DockerOptions, limit time.Duration) (int, string) {
	return runDocker(ctx, ex, o.Name, DockerArgs(o), limit)
}

// runDocker is RunContainer for any `docker run` argument list that names its container.
func runDocker(ctx context.Context, ex Execer, name string, args []string, limit time.Duration) (int, string) {
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			bg, stop := context.WithTimeout(context.Background(), 2*time.Minute)
			defer stop()
			_, _ = ex.Run(bg, "docker", DockerKillArgs(name)...)
			_, _ = ex.Run(bg, "docker", DockerRemoveArgs(name)...)
		})
	}
	defer cleanup()
	code, err := ex.Run(runCtx, "docker", args...)
	if err != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return 1, "the campaign deadline passed: the container was killed"
		case ctx.Err() != nil:
			return 1, "interrupted: the container was killed"
		}
		return 1, "docker run failed: " + err.Error()
	}
	return MapDockerExit(code)
}

// MapDockerExit maps the container's exit code: the inner tool's 0, 1 and 2 pass through;
// docker's own 125 to 127 and the kill code 137, and anything else, are exit 1 with the cause.
func MapDockerExit(code int) (int, string) {
	switch code {
	case 0, 1, 2:
		return code, ""
	case 125, 126, 127:
		return 1, fmt.Sprintf("docker could not run the container (exit %d)", code)
	case 137:
		return 1, "the container was killed (exit 137: out of memory or docker kill)"
	}
	return 1, fmt.Sprintf("unexpected container exit code %d", code)
}

// DockerVersionArgs is the argument list of the `go version` check of an image: the same
// bounds as a campaign run (no network, memory, process and CPU limits, no mount, no
// privilege), a recorded name and --rm, so it can be killed by name like any container.
func DockerVersionArgs(image, name string) []string {
	return []string{
		"run", "--rm", "--name", name,
		"--memory", "1g", "--memory-swap", "1g", "--cpus", "1", "--pids-limit", "256",
		"--network", "none",
		image, "go", "version",
	}
}

// VersionContainerName builds the name of the version-check container from numbers only.
func VersionContainerName(now time.Time, pid int) string {
	return fmt.Sprintf("minutiae-fuzz-check-%d-%d", now.Unix(), pid)
}

// ImageGoVersion runs `go version` in the image under a context of at most limit and returns
// its output. On every way out (normal exit, deadline, interrupt, error) it runs `docker kill`
// and `docker rm -f` by the recorded name, once.
func ImageGoVersion(ctx context.Context, image, name string, limit time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	defer func() {
		bg, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		_, _ = dockerOutput(bg, "docker", DockerKillArgs(name)...)
		_, _ = dockerOutput(bg, "docker", DockerRemoveArgs(name)...)
	}()
	out, err := dockerOutput(runCtx, "docker", DockerVersionArgs(image, name)...)
	if err != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("the version check passed its %s limit: the container was killed", limit)
		case ctx.Err() != nil:
			return "", errors.New("interrupted: the version-check container was killed")
		}
		return "", err
	}
	return out, nil
}
