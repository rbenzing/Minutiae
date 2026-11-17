package cli

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The layout of the real device samples for recovery (3K-R5) and its refusals. This file has no build tag, so the
// default tests check the loading; only the command runs (realimages_recovery_test.go) need -tags realimages and
// $MINUTIAE_TEST_IMAGES. Two layouts:
//
//	$MINUTIAE_TEST_IMAGES/recovery/<name>.<img|img.gz|E01> with <name>.expect.json
//	$MINUTIAE_TEST_IMAGES/recovery-db/<set>/expect.json, the database, and its <db>-wal and <db>-journal
//
// Expect files are decoded strictly, so a misspelled key fails instead of disabling a check. A layout error names
// the position and kind of the entry, never its name; no command output of a sample (which may be personal data) is
// ever logged.

// rxBound is an inclusive bound on a count. A nil Min or Max is not checked.
type rxBound struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
}

func (b rxBound) check(n int) error {
	if b.Min != nil && n < *b.Min {
		return fmt.Errorf("count %d is below min %d", n, *b.Min)
	}
	if b.Max != nil && n > *b.Max {
		return fmt.Errorf("count %d is above max %d", n, *b.Max)
	}
	return nil
}

type rxRecoverStep struct {
	rxBound
	Methods []string `json:"methods"` // the allowed set: every new artifact's method must be in it
	SHA256  []string `json:"sha256"`  // contents that must each appear among the new artifacts
}

type rxCarveStep struct {
	rxBound
	TypesMin map[string]int `json:"types_min"` // a carve type -> its minimum count
}

type rxUseJournalStep struct {
	rxBound
	Mode string `json:"mode"` // none, committed or stale
}

type rxSteps struct {
	Recover     *rxRecoverStep    `json:"recover"`
	Carve       *rxCarveStep      `json:"carve"`
	Slack       *rxBound          `json:"slack"`
	Journal     *rxBound          `json:"journal"`
	UseJournal  *rxUseJournalStep `json:"use_journal"`
	Rollforward *rxBound          `json:"rollforward"`
}

func (s rxSteps) count() int {
	n := 0
	for _, p := range []bool{s.Recover != nil, s.Carve != nil, s.Slack != nil, s.Journal != nil, s.UseJournal != nil, s.Rollforward != nil} {
		if p {
			n++
		}
	}
	return n
}

func (b rxBound) set() bool { return b.Min != nil || b.Max != nil }

// unbounded is a typed error for a declared step that carries no bound or constraint: it would pass whatever the
// command did, so it checks nothing.
func (s rxSteps) unbounded() error {
	var empty []string
	if r := s.Recover; r != nil && !r.set() && len(r.Methods) == 0 && len(r.SHA256) == 0 {
		empty = append(empty, "recover")
	}
	if c := s.Carve; c != nil && !c.set() && len(c.TypesMin) == 0 {
		empty = append(empty, "carve")
	}
	if b := s.Slack; b != nil && !b.set() {
		empty = append(empty, "slack")
	}
	if b := s.Journal; b != nil && !b.set() {
		empty = append(empty, "journal")
	}
	if u := s.UseJournal; u != nil && !u.set() {
		empty = append(empty, "use_journal")
	}
	if b := s.Rollforward; b != nil && !b.set() {
		empty = append(empty, "rollforward")
	}
	if len(empty) > 0 {
		return fmt.Errorf("steps %s declares no min, max or constraint, so it would check nothing", strings.Join(empty, ", "))
	}
	return nil
}

var rxUndecidedLine = regexp.MustCompile(`(?m)^undecided [0-9]+ \(not recovered `)

// rxExit1Accepted is whether a step's exit 1 is one the harness tolerates: the recover commands' own undecided-count
// line ("undecided N (not recovered and not shown absent; ...)") or the unsupported-filesystem message. A bare
// mention of the word is not enough.
func rxExit1Accepted(code int, out string) bool {
	return code == 1 && (rxUndecidedLine.MatchString(out) || strings.Contains(out, "does not support recovery"))
}

type rxImageExpect struct {
	ImageSHA256 string  `json:"image_sha256"`
	Platform    string  `json:"platform"`
	Partition   *int    `json:"partition"`
	FSType      string  `json:"fstype"`
	Steps       rxSteps `json:"steps"`
}

type rxDBExpect struct {
	DB                  string             `json:"db"`
	DBSHA256            string             `json:"db_sha256"`
	Platform            string             `json:"platform"`
	SecureDelete        *bool              `json:"secure_delete"`
	Written             *rxBound           `json:"written"`
	Methods             map[string]rxBound `json:"methods"`
	FreelistPagesZeroed *rxBound           `json:"freelist_pages_zeroed"`
}

type rxImageSet struct {
	Name   string // the name inside the layout, for errors
	Path   string // the image (the first segment of an E01)
	Gz     bool
	Expect rxImageExpect
}

type rxDBSet struct {
	Name   string
	Dir    string
	Expect rxDBExpect
}

var rxHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rxPathless is err without the path of a *fs.PathError: a sample's path is never logged.
func rxPathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// rxDecodeStrict decodes one JSON object and refuses an unknown key and trailing data.
func rxDecodeStrict(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // an operator-supplied sample layout
	if err != nil {
		return rxPathless(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON object")
	}
	return nil
}

func rxPlatformOK(p string) bool { return p == "android" || p == "ios" || p == "other" }

func (e rxImageExpect) validate() error {
	switch {
	case !rxHex64.MatchString(e.ImageSHA256):
		return errors.New("image_sha256 is missing or not 64 lower-case hex digits")
	case !rxPlatformOK(e.Platform):
		return fmt.Errorf("platform %q is not android, ios or other", e.Platform)
	case e.FSType == "":
		return errors.New("fstype is missing")
	case e.Partition != nil && *e.Partition < 0:
		return errors.New("partition is negative")
	case e.Steps.count() == 0:
		return errors.New("steps names no step")
	}
	if err := e.Steps.unbounded(); err != nil {
		return err
	}
	if u := e.Steps.UseJournal; u != nil && u.Mode != "none" && u.Mode != "committed" && u.Mode != "stale" {
		return fmt.Errorf("use_journal mode %q is not none, committed or stale", u.Mode)
	}
	if r := e.Steps.Recover; r != nil {
		for _, h := range r.SHA256 {
			if !rxHex64.MatchString(h) {
				return errors.New("recover.sha256 holds a value that is not 64 lower-case hex digits")
			}
		}
	}
	return nil
}

func (e rxDBExpect) validate() error {
	switch {
	case e.DB == "" || e.DB != filepath.Base(e.DB) || strings.ContainsAny(e.DB, `/\`) || e.DB == "." || e.DB == "..":
		return errors.New("db is missing or not a plain file name")
	case !rxHex64.MatchString(e.DBSHA256):
		return errors.New("db_sha256 is missing or not 64 lower-case hex digits")
	case !rxPlatformOK(e.Platform):
		return fmt.Errorf("platform %q is not android, ios or other", e.Platform)
	case e.SecureDelete == nil:
		return errors.New("secure_delete is missing")
	}
	return nil
}

// rxImageSuffixes are the image forms, longest first.
var rxImageSuffixes = []string{".img.gz", ".img", ".E01"}

var rxEWFSegment = regexp.MustCompile(`\.E(0[2-9]|[1-9][0-9])$`)

// rxLoadImageSets reads <root>/recovery. A missing directory gives no sets. Every file must be an image, the
// expect file of an image, or a later E01 segment, and every image and expect file must have its partner.
func rxLoadImageSets(root string) ([]rxImageSet, error) {
	dir := filepath.Join(root, "recovery")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, rxPathless(err)
	}
	images := map[string]rxImageSet{}
	imgPos := map[string]int{}
	// A layout error names an entry by its position (the directory is read in name order) and kind, never by its
	// name: a sample file name may identify a person, and the logs never carry one.
	at := func(i int, kind string) string {
		return fmt.Sprintf("recovery entry %d of %d (%s)", i+1, len(ents), kind)
	}
	expects := map[string]int{}
	for i, e := range ents {
		n := e.Name()
		if e.IsDir() {
			return nil, fmt.Errorf("%s: a directory has no place in the layout", at(i, "a directory"))
		}
		switch base, ok := strings.CutSuffix(n, ".expect.json"); {
		case ok:
			expects[base] = i
			continue
		case rxEWFSegment.MatchString(n):
			continue
		default:
			matched := false
			for _, suf := range rxImageSuffixes {
				if base, ok := strings.CutSuffix(n, suf); ok && base != "" {
					if _, dup := images[base]; dup {
						return nil, fmt.Errorf("%s: the name is used by an earlier image (entry %d)", at(i, "an image"), imgPos[base]+1)
					}
					imgPos[base] = i
					images[base] = rxImageSet{Name: at(i, "an image"), Path: filepath.Join(dir, n), Gz: suf == ".img.gz"}
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("%s: not an image (.img, .img.gz, .E01), an expect file (.expect.json) or an E01 segment", at(i, "a stray file"))
			}
		}
	}
	var names []string
	for b := range images {
		names = append(names, b)
	}
	for b, i := range expects {
		if _, ok := images[b]; !ok {
			return nil, fmt.Errorf("%s: no image of that name", at(i, "an .expect.json"))
		}
	}
	sort.Strings(names)
	var out []rxImageSet
	for _, b := range names {
		set := images[b]
		if _, ok := expects[b]; !ok {
			return nil, fmt.Errorf("%s: no .expect.json of that name", set.Name)
		}
		ep := filepath.Join(dir, b+".expect.json")
		if err := rxDecodeStrict(ep, &set.Expect); err != nil {
			return nil, fmt.Errorf("%s: %w", at(expects[b], "an .expect.json"), err)
		}
		if err := set.Expect.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", at(expects[b], "an .expect.json"), err)
		}
		out = append(out, set)
	}
	return out, nil
}

// rxLoadDBSets reads <root>/recovery-db/<set>/. A missing directory gives no sets.
func rxLoadDBSets(root string) ([]rxDBSet, error) {
	dir := filepath.Join(root, "recovery-db")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, rxPathless(err)
	}
	var out []rxDBSet
	for i, e := range ents {
		name := fmt.Sprintf("recovery-db entry %d of %d", i+1, len(ents))
		if !e.IsDir() {
			return nil, fmt.Errorf("%s: a file has no place beside the sets", name)
		}
		set := rxDBSet{Name: name, Dir: filepath.Join(dir, e.Name())}
		if err := rxDecodeStrict(filepath.Join(set.Dir, "expect.json"), &set.Expect); err != nil {
			return nil, fmt.Errorf("%s (its expect.json): %w", name, err)
		}
		if err := set.Expect.validate(); err != nil {
			return nil, fmt.Errorf("%s (its expect.json): %w", name, err)
		}
		allowed := map[string]bool{"expect.json": true, set.Expect.DB: true, set.Expect.DB + "-wal": true, set.Expect.DB + "-journal": true}
		files, err := os.ReadDir(set.Dir)
		if err != nil {
			return nil, rxPathless(err)
		}
		have := map[string]bool{}
		for j, f := range files {
			if !allowed[f.Name()] || f.IsDir() {
				return nil, fmt.Errorf("%s, file %d of %d (a stray file): not the database, its -wal or -journal, or expect.json", name, j+1, len(files))
			}
			have[f.Name()] = true
		}
		if !have[set.Expect.DB] {
			return nil, fmt.Errorf("%s (its expect.json): names a database that is not in the set", name)
		}
		out = append(out, set)
	}
	return out, nil
}

// rxResolve makes p absolute with symlinks resolved, through its deepest existing ancestor.
func rxResolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// rxRootError is the error for a sample root that resolves inside the repository: real evidence beside tracked
// files is one `git add` from a commit.
func rxRootError(root, repo string) error {
	r, err := rxResolve(root)
	if err != nil {
		return err
	}
	g, err := rxResolve(repo)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(g, r); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("MINUTIAE_TEST_IMAGES is inside the repository; real samples must live outside it and are never committed")
	}
	return nil
}

// rxRepoRoot walks up from the package directory to go.mod.
func rxRepoRoot() (string, error) {
	d, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		p := filepath.Dir(d)
		if p == d {
			return "", errors.New("no go.mod above the package directory")
		}
		d = p
	}
}

// rxCheckSHA256 streams the file (decompressing a .gz) and compares its SHA-256 with want.
func rxCheckSHA256(path, want string, gz bool) error {
	f, err := os.Open(path) //nolint:gosec // an operator-supplied sample
	if err != nil {
		return rxPathless(err)
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the sample is not the one the expectation describes: sha256 %s, expectation %s", got, want)
	}
	return nil
}

func rxWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

const rxGoodHash = "0000000000000000000000000000000000000000000000000000000000000000"

func rxGoodImageExpect(extra string) []byte {
	return []byte(`{"image_sha256":"` + rxGoodHash + `","platform":"android","fstype":"ext4","steps":{"recover":{"min":1}}` + extra + `}`)
}

func rxGoodDBExpect(extra string) []byte {
	return []byte(`{"db":"a.db","db_sha256":"` + rxGoodHash + `","platform":"ios","secure_delete":false` + extra + `}`)
}

func TestRealImagesRecoveryRootInsideRepoIsRefused(t *testing.T) {
	repo, err := rxRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	err = rxRootError(filepath.Join(repo, "internal", "cli", "testdata"), repo)
	if err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("a root inside the repository: %v, want an error naming the reason", err)
	}
	if err := rxRootError(repo, repo); err == nil {
		t.Error("the repository root itself is accepted")
	}
	if err := rxRootError(t.TempDir(), repo); err != nil {
		t.Errorf("a root outside the repository: %v", err)
	}
	// a sibling whose name only starts like the repository is outside it
	if err := rxRootError(repo+"-samples", repo); err != nil {
		t.Errorf("a sibling directory: %v", err)
	}
}

func TestRealImagesRecoveryLayoutRefusesTypos(t *testing.T) {
	type tc struct {
		name  string
		files map[string][]byte
		db    bool
		want  string // a substring of the error, which also names the file
	}
	cases := []tc{
		{"unknown key", map[string][]byte{"recovery/john-doe-phone.img": {1}, "recovery/john-doe-phone.expect.json": rxGoodImageExpect(`,"recovr":{}`)}, false, "entry 1 of 2 (an .expect.json)"},
		{"image without expect", map[string][]byte{"recovery/john-doe-phone.img": {1}}, false, "entry 1 of 1 (an image)"},
		{"expect without image", map[string][]byte{"recovery/john-doe-phone.expect.json": rxGoodImageExpect("")}, false, "entry 1 of 1 (an .expect.json)"},
		{"empty steps", map[string][]byte{"recovery/john-doe-phone.img": {1}, "recovery/john-doe-phone.expect.json": []byte(`{"image_sha256":"` + rxGoodHash + `","platform":"ios","fstype":"apfs","steps":{}}`)}, false, "entry 1 of 2 (an .expect.json)"},
		{"unknown step key", map[string][]byte{"recovery/john-doe-phone.img": {1}, "recovery/john-doe-phone.expect.json": []byte(`{"image_sha256":"` + rxGoodHash + `","platform":"ios","fstype":"apfs","steps":{"carv":{}}}`)}, false, "entry 1 of 2 (an .expect.json)"},
		{"stray file", map[string][]byte{"recovery/john-doe-phone.img": {1}, "recovery/john-doe-phone.expect.json": rxGoodImageExpect(""), "recovery/john-doe-notes.txt": {1}}, false, "entry 1 of 3 (a stray file)"},
		{"missing db", map[string][]byte{"recovery-db/john-doe-set/expect.json": rxGoodDBExpect("")}, true, "recovery-db entry 1 of 1 (its expect.json)"},
		{"db key typo", map[string][]byte{"recovery-db/john-doe-set/expect.json": rxGoodDBExpect(`,"secure_delet":true`), "recovery-db/john-doe-set/a.db": {1}}, true, "recovery-db entry 1 of 1 (its expect.json)"},
		{"missing secure_delete", map[string][]byte{"recovery-db/john-doe-set/expect.json": []byte(`{"db":"a.db","db_sha256":"` + rxGoodHash + `","platform":"ios"}`), "recovery-db/john-doe-set/a.db": {1}}, true, "recovery-db entry 1 of 1 (its expect.json)"},
		{"stray db file", map[string][]byte{"recovery-db/john-doe-set/expect.json": rxGoodDBExpect(""), "recovery-db/john-doe-set/a.db": {1}, "recovery-db/john-doe-set/john-doe.db-shm": {1}}, true, "file 3 of 3 (a stray file)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for p, b := range c.files {
				rxWrite(t, filepath.Join(root, filepath.FromSlash(p)), b)
			}
			var err error
			if c.db {
				_, err = rxLoadDBSets(root)
			} else {
				_, err = rxLoadImageSets(root)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one naming %q", err, c.want)
			} else if strings.Contains(err.Error(), "john-doe") {
				t.Errorf("error %q carries a sample name", err)
			}
		})
	}
	t.Run("good layout loads", func(t *testing.T) {
		root := t.TempDir()
		rxWrite(t, filepath.Join(root, "recovery", "a.img.gz"), []byte{1})
		rxWrite(t, filepath.Join(root, "recovery", "a.expect.json"), rxGoodImageExpect(""))
		rxWrite(t, filepath.Join(root, "recovery", "e.E01"), []byte{1})
		rxWrite(t, filepath.Join(root, "recovery", "e.E02"), []byte{1})
		rxWrite(t, filepath.Join(root, "recovery", "e.expect.json"), rxGoodImageExpect(`,"partition":1`))
		rxWrite(t, filepath.Join(root, "recovery-db", "s1", "expect.json"), rxGoodDBExpect(`,"written":{"min":1},"methods":{"freelist":{"min":0}}`))
		rxWrite(t, filepath.Join(root, "recovery-db", "s1", "a.db"), []byte{1})
		rxWrite(t, filepath.Join(root, "recovery-db", "s1", "a.db-wal"), []byte{1})
		is, err := rxLoadImageSets(root)
		if err != nil || len(is) != 2 || !is[0].Gz || is[1].Gz || is[1].Expect.Partition == nil || *is[1].Expect.Partition != 1 {
			t.Errorf("image sets %+v: %v", is, err)
		}
		ds, err := rxLoadDBSets(root)
		if err != nil || len(ds) != 1 || ds[0].Expect.Written == nil {
			t.Errorf("db sets %+v: %v", ds, err)
		}
		if is, err := rxLoadImageSets(t.TempDir()); err != nil || len(is) != 0 {
			t.Errorf("an empty root: %v %v", is, err)
		}
	})
}

func TestRealImagesRecoverySHA256CheckedFirst(t *testing.T) {
	data := []byte{1, 2, 3}
	sum := sha256.Sum256(data)
	right := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	plain := filepath.Join(dir, "a.img")
	rxWrite(t, plain, data)
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipped := filepath.Join(dir, "a.img.gz")
	rxWrite(t, zipped, zb.Bytes())
	if err := rxCheckSHA256(filepath.Join(dir, "absent.img"), right, false); err == nil || strings.Contains(err.Error(), dir) {
		t.Errorf("a missing sample: %v, want an error without the path", err)
	}
	for _, c := range []struct {
		path string
		gz   bool
	}{{plain, false}, {zipped, true}} {
		if err := rxCheckSHA256(c.path, right, c.gz); err != nil {
			t.Errorf("the right hash: %v", err)
		}
		err := rxCheckSHA256(c.path, rxGoodHash, c.gz)
		if err == nil || !strings.Contains(err.Error(), rxGoodHash) || !strings.Contains(err.Error(), right) {
			t.Errorf("a wrong hash: %v, want both values", err)
		}
	}
}

func TestRxBound(t *testing.T) {
	one, three := 1, 3
	zero := 0
	for _, c := range []struct {
		b    rxBound
		n    int
		fail bool
	}{
		{rxBound{Min: &one}, 0, true},
		{rxBound{Min: &one}, 1, false},
		{rxBound{Min: &one}, 99, false},
		{rxBound{Max: &three}, 3, false},
		{rxBound{Max: &three}, 4, true},
		{rxBound{Min: &one, Max: &three}, 0, true},
		{rxBound{Min: &one, Max: &three}, 2, false},
		{rxBound{Min: &one, Max: &three}, 4, true},
		{rxBound{}, -5, false},
		{rxBound{}, 1 << 20, false},
		{rxBound{Max: &zero}, 0, false},
		{rxBound{Max: &zero}, 1, true},
	} {
		err := c.b.check(c.n)
		if (err != nil) != c.fail {
			t.Errorf("%+v check(%d): %v, want failure %v", c.b, c.n, err, c.fail)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprint(c.n)) {
			t.Errorf("error %q does not name the count %d", err, c.n)
		}
	}
}

func TestRealImagesRecoveryDeclaredStepMustCheckSomething(t *testing.T) {
	steps := func(s string) []byte {
		return []byte(`{"image_sha256":"` + rxGoodHash + `","platform":"android","fstype":"ext4","steps":` + s + `}`)
	}
	for _, c := range []struct {
		name, steps string
		fail        bool
	}{
		{"recover empty", `{"recover":{}}`, true},
		{"carve empty", `{"carve":{}}`, true},
		{"slack empty", `{"slack":{}}`, true},
		{"journal empty", `{"journal":{}}`, true},
		{"use_journal mode only", `{"use_journal":{"mode":"none"}}`, true},
		{"rollforward empty", `{"rollforward":{}}`, true},
		{"one good, one empty", `{"recover":{"min":1},"slack":{}}`, true},
		{"recover min", `{"recover":{"min":0}}`, false},
		{"recover max", `{"recover":{"max":0}}`, false},
		{"recover methods", `{"recover":{"methods":["x"]}}`, false},
		{"recover sha256", `{"recover":{"sha256":["` + rxGoodHash + `"]}}`, false},
		{"carve types_min", `{"carve":{"types_min":{"jpeg":1}}}`, false},
		{"use_journal bound", `{"use_journal":{"mode":"stale","min":1}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			rxWrite(t, filepath.Join(root, "recovery", "x.img"), []byte{1})
			rxWrite(t, filepath.Join(root, "recovery", "x.expect.json"), steps(c.steps))
			_, err := rxLoadImageSets(root)
			if c.fail != (err != nil) {
				t.Fatalf("error %v, want failure %v", err, c.fail)
			}
			if err != nil && !strings.Contains(err.Error(), "would check nothing") {
				t.Errorf("error %q does not say the step checks nothing", err)
			}
		})
	}
}

func TestRxExit1Accepted(t *testing.T) {
	for _, c := range []struct {
		code int
		out  string
		want bool
	}{
		{1, "recovered 2\nundecided 3 (not recovered and not shown absent; see the skipped lines)\n", true},
		{1, "this image does not support recovery", true},
		{1, "an undecided thing happened", false},
		{1, "the word undecided alone", false},
		{1, "undecided (not recovered", false},
		{1, "", false},
		{0, "undecided 3 (not recovered and not shown absent)\n", false},
		{4, "undecided 3 (not recovered and not shown absent)\n", false},
	} {
		if got := rxExit1Accepted(c.code, c.out); got != c.want {
			t.Errorf("rxExit1Accepted(%d, %q) = %v, want %v", c.code, c.out, got, c.want)
		}
	}
}
