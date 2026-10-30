//go:build realdata

package cli

import (
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// The real-data command line tests (4H-R10). They run only with -tags realdata and $MINUTIAE_TEST_DATA; without
// data every test SKIPs with the reason. Running them is an OPERATOR step, never a gate (docs/testing.md "Real
// data"). The layout is the one internal/parsers/realdata_inputs_test.go reads (parsers/<parser>/<set>/ and
// ios-backup/<set>/); the table below repeats its logical paths and file names because a test of another package
// cannot import them, and every set is checked against the parser's primary glob before it is used. Nothing here
// prints personal data: the logs name parsers, outcomes, counts and reasons.

// realCLIInput is the device name of a parser's database: its logical path and the extra roles found by file name.
type realCLIInput struct {
	logical, file string
	extra         map[string]string
}

var realCLIInputs = map[string]realCLIInput{
	"android.mmssms":   {logical: "android:/data/data/com.android.providers.telephony/databases/mmssms.db", file: "mmssms.db"},
	"android.calllog":  {logical: "android:/data/data/com.android.providers.contacts/databases/calllog.db", file: "calllog.db"},
	"android.contacts": {logical: "android:/data/data/com.android.providers.contacts/databases/contacts2.db", file: "contacts2.db"},
	"ios.sms":          {logical: "ios:HomeDomain/Library/SMS/sms.db", file: "sms.db"},
	"ios.calls":        {logical: "ios:HomeDomain/Library/CallHistoryDB/CallHistory.storedata", file: "CallHistory.storedata"},
	"ios.addressbook": {logical: "ios:HomeDomain/Library/AddressBook/AddressBook.sqlitedb", file: "AddressBook.sqlitedb",
		extra: map[string]string{"images": "AddressBookImages.sqlitedb", "images_wal": "AddressBookImages.sqlitedb-wal"}},
	"chromium.history": {logical: "android:/data/data/com.android.chrome/app_chrome/Default/History", file: "History"},
	"safari.history":   {logical: "ios:HomeDomain/Library/Safari/History.db", file: "History.db"},
}

// realCLIFile is one file of a set: its name and the logical path it has on the device.
type realCLIFile struct{ src, logical string }

// realCLISet is one parser set: the parser, its device platform and the files to capture (the database first).
type realCLISet struct {
	parser, name, platform string
	files                  []realCLIFile
}

func realCLIDataRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("MINUTIAE_TEST_DATA")
	if root == "" {
		t.Skip("MINUTIAE_TEST_DATA is not set")
	}
	return root
}

func realCLISubdirs(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func realCLIFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// realCLISets reads the parsers/ part of the layout; a directory that names no registered parser or a set
// without its database fails the test (a typo is never a silent skip).
func realCLISets(t *testing.T, root string) []realCLISet {
	t.Helper()
	var out []realCLISet
	for _, name := range realCLISubdirs(t, filepath.Join(root, "parsers")) {
		in, ok := realCLIInputs[name]
		if !ok {
			t.Fatalf("%s: %q names no registered parser", filepath.Join(root, "parsers"), name)
		}
		var rm parse.Meta
		for _, p := range parsers.All() {
			if p.Meta().Name == name {
				rm = p.Meta()
			}
		}
		for _, set := range realCLISubdirs(t, filepath.Join(root, "parsers", name)) {
			dir := filepath.Join(root, "parsers", name, set)
			logical, file := in.logical, in.file
			if raw, err := os.ReadFile(filepath.Join(dir, "logical")); err == nil {
				logical = strings.TrimSpace(string(raw))
				file = path.Base(logical)
			}
			if !realCLIFileExists(filepath.Join(dir, file)) {
				t.Fatalf("%s: the set holds no database file %q", dir, file)
			}
			matched := false
			for _, gl := range rm.Inputs[0].Globs {
				if g, err := parse.CompileGlob(gl); err == nil && g.Match(logical) {
					matched = true
				}
			}
			if !matched {
				t.Fatalf("%s: logical path %q matches none of the primary globs %v of %s", dir, logical, rm.Inputs[0].Globs, name)
			}
			s := realCLISet{parser: name, name: set, platform: strings.SplitN(logical, ":", 2)[0]}
			s.files = append(s.files, realCLIFile{filepath.Join(dir, file), logical})
			for _, suffix := range []string{"-wal", "-journal"} {
				if realCLIFileExists(filepath.Join(dir, file+suffix)) {
					s.files = append(s.files, realCLIFile{filepath.Join(dir, file+suffix), logical + suffix})
				}
			}
			for _, extra := range slices.Sorted(maps.Values(in.extra)) {
				if realCLIFileExists(filepath.Join(dir, extra)) {
					s.files = append(s.files, realCLIFile{filepath.Join(dir, extra), path.Dir(logical) + "/" + extra})
				}
			}
			out = append(out, s)
		}
	}
	return out
}

// device remote path of a logical path: Android keeps the absolute path, iOS keeps what follows the domain (the
// device-file namer puts such a file under MediaDomain, which the first-set globs accept as any domain).
func realCLIRemote(logical string) string {
	if p, ok := strings.CutPrefix(logical, "android:"); ok {
		return p
	}
	rest := strings.TrimPrefix(logical, "ios:")
	_, rel, _ := strings.Cut(rest, "/")
	return rel
}

type realCLIRun struct {
	code        int
	out, errOut string
}

func realCLI(t *testing.T, args ...string) realCLIRun {
	t.Helper()
	var r realCLIRun
	r.code, r.out, r.errOut = runP(t, nil, args...)
	t.Logf("minutiae %s: exit %d", strings.Join(args[:min(len(args), 2)], " "), r.code)
	if r.code == ExitIntegrity {
		t.Errorf("minutiae %s: exit %d (integrity)\n%s\n%s", strings.Join(args, " "), r.code, r.out, r.errOut)
	}
	return r
}

// A case holding the set's files as file artifacts of one device: parse plan, parse run, sqlite recover on the
// database and case verify. No exit 4 anywhere; case verify exits 0 (the parse-job cross-check included); exit 1
// of a parse command is allowed only with its reasons, which are logged.
func TestRealDataEndToEnd(t *testing.T) {
	root := realCLIDataRoot(t)
	sets := realCLISets(t, root)
	if len(sets) == 0 {
		t.Skip("the layout holds no parser set")
	}
	for _, s := range sets {
		t.Run(s.parser+"/"+s.name, func(t *testing.T) {
			for _, f := range s.files {
				t.Logf("captured %s: logical path of the layout %s, remote path %s (an iOS file is named ios:MediaDomain/<remote path> by the device-file namer)", filepath.Base(f.src), f.logical, realCLIRemote(f.logical))
			}
			c := recordstest.NewCase(t)
			acq := "logical"
			if s.platform == "ios" {
				acq = "ios.backup" // device D1 is an iOS device: its file artifacts have platform ios
			}
			if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": acq}); err != nil {
				t.Fatal(err)
			}
			var dbID string
			for i, f := range s.files {
				data, err := os.ReadFile(f.src)
				if err != nil {
					t.Fatal(err)
				}
				rec := pcapture(t, c, "files/"+path.Base(f.src)+"."+string(rune('a'+i)), evidence.Source{Kind: "file", RemotePath: realCLIRemote(f.logical)}, string(data))
				if i == 0 {
					dbID = rec.ID
				}
			}
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if r := realCLI(t, "parse", "plan", "--case", dir); r.code != 0 {
				t.Errorf("parse plan: exit %d\n%s\n%s", r.code, r.out, r.errOut)
			}
			run := realCLI(t, "parse", "run", "--case", dir, "--json")
			if run.code != 0 && run.code != 1 {
				t.Errorf("parse run: exit %d\n%s\n%s", run.code, run.out, run.errOut)
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(run.out), "\n") {
				var j jobJSON
				if json.Unmarshal([]byte(line), &j) != nil || j.Parser == "" || j.Outcome == "" { // the job events with an outcome
					continue
				}
				t.Logf("job %d %s: %s %s (records %d, rejected %d, warnings %d)", j.Job, j.Parser, j.Outcome, j.Reason, j.Records, j.Rejected, j.Warnings)
				found = found || j.Parser == s.parser
			}
			if !found {
				t.Errorf("parse run made no job for %s over its own database (exit %d):\n%s\n%s", s.parser, run.code, run.out, run.errOut)
			}
			rec := realCLI(t, "sqlite", "recover", "--case", dir, "--json", dbID)
			if rec.code != 0 && rec.code != 1 {
				t.Errorf("sqlite recover: exit %d\n%s\n%s", rec.code, rec.out, rec.errOut)
			}
			if rec.code == 1 {
				t.Logf("sqlite recover exit 1: %s", strings.TrimSpace(rec.errOut))
			}
			assertTypedRecordsAreRecovered(t, dir)
			if v := realCLI(t, "case", "verify", "--case", dir); v.code != 0 {
				t.Errorf("case verify: exit %d\n%s\n%s", v.code, v.out, v.errOut)
			}
		})
	}
}

// Each ios-backup/<set>/ directory, file by file, as backup artifacts (the remote path is the path relative to the
// set, under the set name, as the acquisition writes it): parse plan (the BACKUP lines are logged), parse run, parse
// recover and case verify. An encrypted backup ends unparsed with its reason and exit 1, never exit 0 with nothing.
func TestRealDataIOSBackup(t *testing.T) {
	root := realCLIDataRoot(t)
	sets := realCLISubdirs(t, filepath.Join(root, "ios-backup"))
	if len(sets) == 0 {
		t.Skip("the layout holds no ios-backup set")
	}
	for _, name := range sets {
		t.Run(name, func(t *testing.T) {
			setDir := filepath.Join(root, "ios-backup", name)
			if !realCLIFileExists(filepath.Join(setDir, "Manifest.db")) && !realCLIFileExists(filepath.Join(setDir, "Manifest.mbdb")) {
				t.Fatalf("%s: a backup set holds no Manifest.db or Manifest.mbdb", setDir)
			}
			c := recordstest.NewCase(t)
			if _, err := c.Audit.Append("acquire.start", "D1", map[string]any{"acquisition_id": "A1", "type": "ios.backup"}); err != nil {
				t.Fatal(err)
			}
			n := 0
			err := filepath.WalkDir(setDir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, err := filepath.Rel(setDir, p)
				if err != nil {
					return err
				}
				remote := name + "/" + filepath.ToSlash(rel)
				src := evidence.Source{Kind: "backup", DeviceID: "D1", RemotePath: remote}
				_, err = c.Capture("D1", "A1", "backup/"+remote, src, func(w io.Writer) error {
					f, err := os.Open(p) //nolint:gosec // an operator-supplied data set
					if err != nil {
						return err
					}
					defer func() { _ = f.Close() }()
					_, err = io.Copy(w, f)
					return err
				})
				n++
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := c.Dir
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("%d files captured", n)
			plan := realCLI(t, "parse", "plan", "--case", dir)
			if plan.code != 0 {
				t.Errorf("parse plan: exit %d\n%s\n%s", plan.code, plan.out, plan.errOut)
			}
			for _, line := range strings.Split(plan.out, "\n") {
				if strings.HasPrefix(line, "BACKUP ") {
					t.Log(line)
				}
			}
			run := realCLI(t, "parse", "run", "--case", dir, "--json")
			switch run.code {
			case 0:
				jobs := 0
				for _, line := range strings.Split(strings.TrimSpace(run.out), "\n") {
					var j jobJSON
					if json.Unmarshal([]byte(line), &j) == nil && j.Parser != "" && j.Outcome != "" {
						jobs++
						t.Logf("job %d %s: %s %s", j.Job, j.Parser, j.Outcome, j.Reason)
					}
				}
				if jobs == 0 {
					t.Errorf("parse run exits 0 with no job and no unparsed reason:\n%s\n%s", run.out, run.errOut)
				}
			case 1:
				t.Logf("parse run exit 1 (unparsed or incomplete); reasons:\n%s\n%s", run.out, run.errOut)
			default:
				t.Errorf("parse run: exit %d\n%s\n%s", run.code, run.out, run.errOut)
			}
			recov := realCLI(t, "parse", "recover", "--case", dir, "--json")
			if recov.code != 0 && recov.code != 1 {
				t.Errorf("parse recover: exit %d\n%s\n%s", recov.code, recov.out, recov.errOut)
			}
			if v := realCLI(t, "case", "verify", "--case", dir); v.code != 0 {
				t.Errorf("case verify: exit %d\n%s\n%s", v.code, v.out, v.errOut)
			}
		})
	}
}

// assertTypedRecordsAreRecovered reads back every typed record the case holds after `sqlite recover` (splitCase) and
// requires of each: deleted, recovered, a recovery method, a confidence within 0..100 (the method's own cap is not
// reachable from this package), and no appearance in the live view (--recovered none). Recovered data must never be
// presented as live; the host, not the mapper, stamps these facts.
func assertTypedRecordsAreRecovered(t *testing.T, dir string) {
	t.Helper()
	s := splitCase(t, dir)
	names := map[string]bool{}
	for _, r := range s.typedAll {
		names[r.Parser.Name] = true
		if !r.Deleted || !r.Recovered || r.RecoveryMethod == "" || r.Confidence == nil || *r.Confidence < 0 || *r.Confidence > 100 {
			conf := "none"
			if r.Confidence != nil {
				conf = strconv.Itoa(*r.Confidence)
			}
			t.Errorf("typed record %s (%s) is not a stamped recovered record: deleted %v, recovered %v, method %q, confidence %s",
				r.Locator, r.Type, r.Deleted, r.Recovered, r.RecoveryMethod, conf)
		}
	}
	t.Logf("%d typed recovered records, %d generic db_rows", len(s.typedAll), len(s.db))
	for name := range names {
		code, out, _ := runP(t, nil, "records", "list", "--case", dir, "--json", "--limit", "1", "--recovered", "none", "--parser", name)
		var l struct {
			Records []rowJSON `json:"records"`
		}
		if code != 0 || json.Unmarshal(jsonPart(out), &l) != nil {
			t.Errorf("records list --recovered none --parser %s: exit %d\n%s", name, code, out)
		} else if len(l.Records) != 0 {
			t.Errorf("the live view holds a record of the recovery parser %s (%s)", name, l.Records[0].Locator)
		}
	}
}
