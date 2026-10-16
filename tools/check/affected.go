package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/build/constraint"
	"io"
	"path"
	"sort"
	"strings"
)

// testTimeout is the explicit go test timeout of every mode.
const testTimeout = "30m"

// pkg is one package of the module as the affected-set rules see it.
type pkg struct {
	Dir    string   // module-relative, slash separated ("internal/archtest")
	Deps   []string // in-module packages named by Imports, TestImports or XTestImports (Dir form)
	Embeds []string // module-relative files embedded by the package or its tests
}

// goListPackage is the part of `go list -json` the rules use.
type goListPackage struct {
	Dir             string
	ImportPath      string
	Imports         []string
	TestImports     []string
	XTestImports    []string
	EmbedFiles      []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

// parseGoList reads the concatenated JSON objects of `go list -json ./...` for the module modPath
// rooted at the absolute directory root.
func parseGoList(data []byte, modPath, root string) ([]pkg, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out []pkg
	for {
		var g goListPackage
		if err := dec.Decode(&g); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("go list -json: %w", err)
		}
		rel, err := relDir(root, g.Dir)
		if err != nil {
			return nil, err
		}
		p := pkg{Dir: rel}
		seen := map[string]bool{rel: true}
		for _, imps := range [][]string{g.Imports, g.TestImports, g.XTestImports} {
			for _, imp := range imps {
				d, ok := strings.CutPrefix(imp, modPath+"/")
				if ok && !seen[d] {
					seen[d] = true
					p.Deps = append(p.Deps, d)
				}
			}
		}
		sort.Strings(p.Deps)
		for _, embeds := range [][]string{g.EmbedFiles, g.TestEmbedFiles, g.XTestEmbedFiles} {
			for _, e := range embeds {
				p.Embeds = append(p.Embeds, path.Join(rel, e))
			}
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("go list -json listed no package")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// affectedClosure returns, sorted, the seed packages plus every package that reaches one of them
// through Imports, TestImports or XTestImports, transitively.
func affectedClosure(pkgs []pkg, seeds map[string]bool) []string {
	rev := map[string][]string{}
	for _, p := range pkgs {
		for _, d := range p.Deps {
			rev[d] = append(rev[d], p.Dir)
		}
	}
	known := map[string]bool{}
	for _, p := range pkgs {
		known[p.Dir] = true
	}
	in := map[string]bool{}
	var queue []string
	for s := range seeds {
		if known[s] && !in[s] {
			in[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range rev[cur] {
			if !in[r] {
				in[r] = true
				queue = append(queue, r)
			}
		}
	}
	out := make([]string, 0, len(in))
	for d := range in {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

var knownGOOS = toSet("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js",
	"linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos")

var knownGOARCH = toSet("386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips",
	"mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv",
	"riscv64", "s390", "s390x", "sparc", "sparc64", "wasm")

func toSet(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// nameIsConstrained reports the implicit _GOOS, _GOARCH and _GOOS_GOARCH file name constraints.
func nameIsConstrained(name string) bool {
	name = strings.TrimSuffix(name, "_test")
	parts := strings.Split(name, "_")
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	return knownGOOS[last] || knownGOARCH[last]
}

// constraintTag returns the first cgo, GOOS, GOARCH or unix tag of the //go:build line (which
// precedes the package clause), or "".
func constraintTag(src []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			return "", nil
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return "", err
		}
		return firstPlatformTag(expr), nil
	}
	return "", sc.Err()
}

func firstPlatformTag(e constraint.Expr) string {
	switch x := e.(type) {
	case *constraint.TagExpr:
		if x.Tag == "cgo" || x.Tag == "unix" || knownGOOS[x.Tag] || knownGOARCH[x.Tag] {
			return x.Tag
		}
	case *constraint.NotExpr:
		return firstPlatformTag(x.X)
	case *constraint.AndExpr:
		if t := firstPlatformTag(x.X); t != "" {
			return t
		}
		return firstPlatformTag(x.Y)
	case *constraint.OrExpr:
		if t := firstPlatformTag(x.X); t != "" {
			return t
		}
		return firstPlatformTag(x.Y)
	}
	return ""
}
