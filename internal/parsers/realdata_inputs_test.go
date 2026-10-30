package parsers

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// This file is the layout of the real-data harness (4H-R10, R26). It is part of the DEFAULT build, so the
// skip and refusal rules run in every default test; the heavy parser tests that use it carry the realdata tag
// (realdata_test.go) and need $MINUTIAE_TEST_DATA.

// realDataInput is how the harness stores one set of a parser as device files: the platform, the logical path
// of the database (it must match the parser's primary glob), the device base name of the database file, and the
// extra roles found by file name next to it.
type realDataInput struct {
	Platform, Logical, File string
	Extra                   map[string]string // role -> file name (ios.addressbook: images, images_wal)
}

// realDataInputs has one entry per registered parser; TestRealDataInputsCoverTheRegistry keeps it complete.
var realDataInputs = map[string]realDataInput{
	"android.mmssms":   {Platform: "android", Logical: "android:/data/data/com.android.providers.telephony/databases/mmssms.db", File: "mmssms.db"},
	"android.calllog":  {Platform: "android", Logical: "android:/data/data/com.android.providers.contacts/databases/calllog.db", File: "calllog.db"},
	"android.contacts": {Platform: "android", Logical: "android:/data/data/com.android.providers.contacts/databases/contacts2.db", File: "contacts2.db"},
	"ios.sms":          {Platform: "ios", Logical: "ios:HomeDomain/Library/SMS/sms.db", File: "sms.db"},
	"ios.calls":        {Platform: "ios", Logical: "ios:HomeDomain/Library/CallHistoryDB/CallHistory.storedata", File: "CallHistory.storedata"},
	"ios.addressbook": {
		Platform: "ios", Logical: "ios:HomeDomain/Library/AddressBook/AddressBook.sqlitedb", File: "AddressBook.sqlitedb",
		Extra: map[string]string{"images": "AddressBookImages.sqlitedb", "images_wal": "AddressBookImages.sqlitedb-wal"},
	},
	"chromium.history": {Platform: "android", Logical: "android:/data/data/com.android.chrome/app_chrome/Default/History", File: "History"},
	"safari.history":   {Platform: "ios", Logical: "ios:HomeDomain/Library/Safari/History.db", File: "History.db"},
}

// realDataSet is one set of the layout: the parser (or "ios-backup") and the files of the set, by role.
type realDataSet struct {
	Parser, Name, Dir, Logical string
	Files                      map[string]string // role -> path of the file in the set (db always present; an ios-backup set has none)
}

const (
	realDataEnv        = "MINUTIAE_TEST_DATA"
	realDataBackupKind = "ios-backup"
)

// subdirs lists the sub-directories of dir by name; a plain file is ignored (a README may sit in a layout).
func subdirs(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil // os.ReadDir sorts by file name
}

func isFile(p string) bool {
	st, err := os.Stat(p) //nolint:gosec // a path of an operator-supplied data set
	return err == nil && st.Mode().IsRegular()
}

// readRealDataLayout reads a layout root (4H-R10): sets in order, a skip reason per parser directory without
// sets, and an error for a directory that names no registered parser or a set without its database file.
func readRealDataLayout(root string) (sets []realDataSet, skipped map[string]string, err error) {
	skipped = map[string]string{}
	tops, err := subdirs(root)
	if err != nil {
		return nil, nil, err
	}
	for _, top := range tops {
		if top != "parsers" && top != realDataBackupKind {
			return nil, nil, fmt.Errorf("%s: %q is neither parsers nor %s: a misspelled directory would be skipped silently", root, top, realDataBackupKind)
		}
	}
	if slices.Contains(tops, "parsers") {
		names, err := subdirs(filepath.Join(root, "parsers"))
		if err != nil {
			return nil, nil, err
		}
		for _, name := range names {
			dir := filepath.Join(root, "parsers", name)
			in, ok := realDataInputs[name]
			if !ok {
				return nil, nil, fmt.Errorf("%s: %q names no registered parser", dir, name)
			}
			setNames, err := subdirs(dir)
			if err != nil {
				return nil, nil, err
			}
			if len(setNames) == 0 {
				skipped[name] = "no sets under " + dir + " (parser " + name + ")"
				continue
			}
			for _, sn := range setNames {
				s, err := readParserSet(name, in, filepath.Join(dir, sn))
				if err != nil {
					return nil, nil, err
				}
				s.Name = sn
				sets = append(sets, s)
			}
		}
	}
	if slices.Contains(tops, realDataBackupKind) {
		names, err := subdirs(filepath.Join(root, realDataBackupKind))
		if err != nil {
			return nil, nil, err
		}
		if len(names) == 0 {
			skipped[realDataBackupKind] = "no sets under " + filepath.Join(root, realDataBackupKind)
		}
		for _, sn := range names {
			dir := filepath.Join(root, realDataBackupKind, sn)
			if !isFile(filepath.Join(dir, "Manifest.db")) && !isFile(filepath.Join(dir, "Manifest.mbdb")) {
				return nil, nil, fmt.Errorf("%s: a backup set holds no Manifest.db or Manifest.mbdb", dir)
			}
			sets = append(sets, realDataSet{Parser: realDataBackupKind, Name: sn, Dir: dir})
		}
	}
	return sets, skipped, nil
}

// readParserSet reads one set directory of a parser: the database under its device base name, its -wal and
// -journal when present, the extra roles by file name and the optional one-line logical override.
func readParserSet(parser string, in realDataInput, dir string) (realDataSet, error) {
	s := realDataSet{Parser: parser, Dir: dir, Logical: in.Logical, Files: map[string]string{}}
	file := in.File
	if raw, err := os.ReadFile(filepath.Join(dir, "logical")); err == nil { //nolint:gosec // a path of an operator-supplied data set
		l := strings.TrimSpace(string(raw))
		if l == "" || strings.ContainsAny(l, "\r\n") {
			return s, fmt.Errorf("%s: the logical file must hold one non-empty line", dir)
		}
		s.Logical, file = l, path.Base(l)
	} else if !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	db := filepath.Join(dir, file)
	if !isFile(db) {
		return s, fmt.Errorf("%s: the set holds no database file %q", dir, file)
	}
	s.Files["db"] = db
	for role, suffix := range map[string]string{"wal": "-wal", "journal": "-journal"} {
		if isFile(db + suffix) {
			s.Files[role] = db + suffix
		}
	}
	for role, name := range in.Extra {
		if isFile(filepath.Join(dir, name)) {
			s.Files[role] = filepath.Join(dir, name)
		}
	}
	return s, nil
}

// realDataSets is readRealDataLayout over $MINUTIAE_TEST_DATA; it skips t when the variable is unset and fails
// t on a layout error.
func realDataSets(t *testing.T) []realDataSet {
	t.Helper()
	root := os.Getenv(realDataEnv)
	if root == "" {
		t.Skip(realDataEnv + " is not set")
	}
	sets, skipped, err := readRealDataLayout(root)
	if err != nil {
		t.Fatalf("the real-data layout under %s: %v", root, err)
	}
	for _, name := range slices.Sorted(maps.Keys(skipped)) {
		t.Logf("skipped: %s", skipped[name])
	}
	if len(sets) == 0 {
		t.Skip("the real-data layout under " + root + " holds no set")
	}
	return sets
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRealDataLayoutRefusesUnknownParserDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "parsers", "android.mmsms", "s1", "mmssms.db"), "x")
	_, _, err := readRealDataLayout(root)
	if err == nil || !strings.Contains(err.Error(), "android.mmsms") {
		t.Fatalf("a misspelled parser directory must fail naming it, got %v", err)
	}
}

func TestRealDataLayoutRefusesUnknownTopLevelDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "parser", "android.mmssms", "s1", "mmssms.db"), "x")
	_, _, err := readRealDataLayout(root)
	if err == nil || !strings.Contains(err.Error(), "parser") {
		t.Fatalf("a misspelled top-level directory must fail naming it, got %v", err)
	}
}

func TestRealDataLayoutRefusesSetWithoutDatabase(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "parsers", "android.mmssms", "s1", "notes.txt"), "x")
	_, _, err := readRealDataLayout(root)
	if err == nil || !strings.Contains(err.Error(), filepath.Join("android.mmssms", "s1")) {
		t.Fatalf("a set without its database must fail naming the set, got %v", err)
	}
	root = t.TempDir()
	writeTestFile(t, filepath.Join(root, "ios-backup", "b1", "readme.txt"), "x")
	if _, _, err = readRealDataLayout(root); err == nil || !strings.Contains(err.Error(), "b1") {
		t.Fatalf("a backup directory without a manifest must fail naming it, got %v", err)
	}
}

func TestRealDataLayoutSkipsAParserWithoutSets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "parsers", "safari.history"), 0o750); err != nil {
		t.Fatal(err)
	}
	sets, skipped, err := readRealDataLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Errorf("sets = %v, want none", sets)
	}
	if !strings.Contains(skipped["safari.history"], "safari.history") {
		t.Errorf("skip reasons = %v, want one naming safari.history", skipped)
	}
}

func TestRealDataLayoutReadsSetsInOrderWithCompanionsAndExtras(t *testing.T) {
	root := t.TempDir()
	ab := filepath.Join(root, "parsers", "ios.addressbook", "b")
	for _, f := range []string{"AddressBook.sqlitedb", "AddressBook.sqlitedb-wal", "AddressBookImages.sqlitedb", "AddressBookImages.sqlitedb-wal"} {
		writeTestFile(t, filepath.Join(ab, f), "x")
	}
	writeTestFile(t, filepath.Join(root, "parsers", "ios.addressbook", "a", "AddressBook.sqlitedb"), "x")
	writeTestFile(t, filepath.Join(root, "parsers", "android.mmssms", "s1", "mmssms.db"), "x")
	writeTestFile(t, filepath.Join(root, "parsers", "android.mmssms", "s1", "mmssms.db-journal"), "x")
	writeTestFile(t, filepath.Join(root, "ios-backup", "bk", "Manifest.db"), "x")
	sets, _, err := readRealDataLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range sets {
		names = append(names, s.Parser+"/"+s.Name)
	}
	want := []string{"android.mmssms/s1", "ios.addressbook/a", "ios.addressbook/b", "ios-backup/bk"}
	if !slices.Equal(names, want) {
		t.Fatalf("sets = %v, want %v", names, want)
	}
	if got := sets[0].Files["journal"]; got != filepath.Join(root, "parsers", "android.mmssms", "s1", "mmssms.db-journal") {
		t.Errorf("journal = %q", got)
	}
	if sets[0].Files["wal"] != "" {
		t.Errorf("a missing -wal must not be a role: %v", sets[0].Files)
	}
	b := sets[2]
	for _, role := range []string{"db", "wal", "images", "images_wal"} {
		if b.Files[role] == "" {
			t.Errorf("addressbook set b has no %s: %v", role, b.Files)
		}
	}
	if sets[1].Files["images"] != "" {
		t.Errorf("addressbook set a has no images file, got %v", sets[1].Files)
	}
	if sets[3].Dir != filepath.Join(root, "ios-backup", "bk") || len(sets[3].Files) != 0 {
		t.Errorf("backup set = %+v", sets[3])
	}
}

func TestRealDataLayoutReadsALogicalOverride(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "parsers", "ios.calls", "s1")
	writeTestFile(t, filepath.Join(dir, "call_history.db"), "x")
	writeTestFile(t, filepath.Join(dir, "logical"), "ios:HomeDomain/Library/CallHistoryDB/call_history.db\n")
	// the override names the database file by its base name: the set holds call_history.db, not the default name
	sets, _, err := readRealDataLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].Logical != "ios:HomeDomain/Library/CallHistoryDB/call_history.db" {
		t.Fatalf("sets = %+v, want the override as the logical path", sets)
	}
	if sets[0].Files["db"] != filepath.Join(dir, "call_history.db") {
		t.Errorf("db = %q", sets[0].Files["db"])
	}
	writeTestFile(t, filepath.Join(dir, "logical"), "ios:HomeDomain/Library/CallHistoryDB/other.db\n")
	if _, _, err = readRealDataLayout(root); err == nil {
		t.Error("an override whose base name is not a file of the set must fail")
	}
}

func TestRealDataInputsCoverTheRegistry(t *testing.T) {
	registered := map[string]parse.Meta{}
	for _, p := range All() {
		registered[p.Meta().Name] = p.Meta()
	}
	if len(registered) == 0 {
		t.Fatal("the registry is empty, so this test would check nothing")
	}
	for name, m := range registered {
		in, ok := realDataInputs[name]
		if !ok {
			t.Errorf("%s is registered but has no realDataInputs entry", name)
			continue
		}
		matched := false
		for _, gl := range m.Inputs[0].Globs {
			g, err := parse.CompileGlob(gl)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if g.Match(in.Logical) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%s: logical %q matches none of the primary globs %v", name, in.Logical, m.Inputs[0].Globs)
		}
		if !strings.HasPrefix(in.Logical, in.Platform+":") || !slices.Contains(m.Platforms, in.Platform) {
			t.Errorf("%s: logical %q has not platform %q of %v", name, in.Logical, in.Platform, m.Platforms)
		}
		if path.Base(in.Logical) != in.File {
			t.Errorf("%s: file %q is not the base name of %q", name, in.File, in.Logical)
		}
		roles := map[string]bool{}
		for _, r := range m.Inputs {
			roles[r.Role] = true
		}
		for role, file := range in.Extra {
			if !roles[role] || file == "" || role == m.Inputs[0].Role {
				t.Errorf("%s: extra role %q (%q) is not a non-primary role of the parser", name, role, file)
			}
		}
	}
	for name := range realDataInputs {
		if _, ok := registered[name]; !ok {
			t.Errorf("realDataInputs names %s, which is not registered", name)
		}
	}
}

func TestRealDataSetsSkipsWithoutTheVariable(t *testing.T) {
	t.Setenv("MINUTIAE_TEST_DATA", "")
	t.Run("inner", func(t *testing.T) {
		realDataSets(t)
		t.Error("realDataSets returned with MINUTIAE_TEST_DATA unset: it must skip")
	})
}
