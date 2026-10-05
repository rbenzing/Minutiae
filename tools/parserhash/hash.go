package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const streamHeader = "minutiae-parser-hash-v1\n"

// HashScope returns "src1:sha256:<hex>" over the scope (format F1). Go files
// (top-level names ending in .go) contribute their normalised syntax tree;
// every other hashed file (an embedded data file) contributes its raw bytes,
// with no line-ending normalisation, because a parser reads those bytes.
func HashScope(s Scope, read func(pkgDir, file string) ([]byte, error)) (string, error) {
	h := sha256.New()
	h.Write([]byte(streamHeader))
	pkgs := append([]ScopePackage(nil), s.Packages...)
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ImportPath < pkgs[j].ImportPath })
	for _, p := range pkgs {
		files := append([]string(nil), p.Files...)
		sort.Strings(files)
		for _, name := range files {
			src, err := read(p.Dir, name)
			if err != nil {
				return "", fmt.Errorf("read %s/%s: %w", p.ImportPath, name, err)
			}
			content := src
			if !strings.Contains(name, "/") && strings.HasSuffix(name, ".go") {
				if content, err = NormalizeGo(src, name); err != nil {
					return "", err
				}
			}
			h.Write([]byte(p.ImportPath + "/" + name + "\x00"))
			h.Write(content)
			h.Write([]byte{0})
		}
	}
	mods := append([]ModuleRef(nil), s.Modules...)
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	for _, m := range mods {
		fmt.Fprintf(h, "module %s %s %s\n", m.Path, m.Version, m.Sum)
	}
	fmt.Fprintf(h, "go %s\n", s.GoDirective)
	return "src1:sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
