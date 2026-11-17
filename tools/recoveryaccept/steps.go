package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// fixture is one committed image: the oracle that describes it sits beside it. A bare filesystem image is read as
// partition 0 (scheme none) and every recovery command chooses the only partition with a recognized filesystem,
// so no step passes -p.
type fixture struct {
	name   string // base name, without .img.gz
	dir    string // under the module root, slash-separated
	oracle string // the oracle file name in dir
}

// fixtures are the seven images, in import order.
var fixtures = []fixture{
	{"fat16-recover", "internal/filesys/fat/testdata/recover", "fat16-recover.expect.json"},
	{"ext4-recover", "internal/filesys/ext4/testdata/recover", "ext4-recover.expect.json"},
	{"f2fs-recover-nodiscard", "internal/filesys/f2fs/testdata/recover", "f2fs-recover-nodiscard.expect.json"},
	{"carve-fat12", "internal/examine/testdata/carve", "carve.expect.json"},
	{"fat16-slack", "internal/filesys/fat/testdata/slack", "fat16-slack.expect.json"},
	{"ext4-journal-crash", "internal/filesys/ext4/testdata/journal", "ext4-journal-crash.expect.json"},
	{"f2fs-rollforward", "internal/filesys/f2fs/testdata/rollforward", "f2fs-rollforward.expect.json"},
}

func (f fixture) imagePath(root string) string {
	return filepath.Join(root, filepath.FromSlash(f.dir), f.name+".img.gz")
}

func (f fixture) oraclePath(root string) string {
	return filepath.Join(root, filepath.FromSlash(f.dir), f.oracle)
}

// maxFixtureBytes bounds a gunzipped fixture (the largest committed image is a few MiB).
const maxFixtureBytes = 1 << 30

// oracleImageSHA is the image_sha256 the oracle states for the named image: the entry of images[] for the carving
// oracle, else the top-level key, else the one under generator.
func oracleImageSHA(raw []byte, name string) (string, error) {
	var o struct {
		ImageSHA256 string `json:"image_sha256"`
		Generator   struct {
			ImageSHA256 string `json:"image_sha256"`
		} `json:"generator"`
		Images []struct {
			Name        string `json:"name"`
			ImageSHA256 string `json:"image_sha256"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", err
	}
	for _, im := range o.Images {
		if im.Name == name && im.ImageSHA256 != "" {
			return im.ImageSHA256, nil
		}
	}
	switch {
	case o.ImageSHA256 != "":
		return o.ImageSHA256, nil
	case o.Generator.ImageSHA256 != "":
		return o.Generator.ImageSHA256, nil
	}
	return "", errors.New("the oracle states no image_sha256")
}

// gunzipTo writes the decompressed bytes of src to a new file dst (never an existing one) and returns their SHA-256.
func gunzipTo(src, dst string) (string, error) {
	in, err := os.Open(src) //nolint:gosec // a committed fixture under the module root
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	w, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // inside the tool's scratch directory
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(zr, maxFixtureBytes+1))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxFixtureBytes {
		return "", fmt.Errorf("the image is larger than %d bytes", maxFixtureBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// jsonValue returns the first JSON object or array in out (the commands print their JSON on stdout).
func jsonValue(out []byte) []byte {
	i := strings.IndexAny(string(out), "{[")
	if i < 0 {
		return nil
	}
	return out[i:]
}

func decodeJSON(out []byte, v any) error {
	j := jsonValue(out)
	if j == nil {
		return errors.New("the output holds no JSON")
	}
	return json.NewDecoder(strings.NewReader(string(j))).Decode(v)
}

// importStep gunzips a fixture, checks it against its oracle and imports it.
func importStep(f fixture) step {
	return step{
		name: "import-" + f.name,
		args: func(s *state) ([]string, error) {
			raw, err := os.ReadFile(f.oraclePath(s.root)) //nolint:gosec // a committed oracle
			if err != nil {
				return nil, err
			}
			want, err := oracleImageSHA(raw, f.name)
			if err != nil {
				return nil, err
			}
			img := filepath.Join(s.scratch, "in", f.name+".img")
			got, err := gunzipTo(f.imagePath(s.root), img)
			if err != nil {
				return nil, err
			}
			if got != want {
				return nil, fmt.Errorf("fixture and oracle differ: the image hashes to %s, the oracle says %s", got, want)
			}
			s.oracle[f.name] = raw
			return []string{"image", "import", "--case", s.caseDir, "--json", img}, nil
		},
		check: func(s *state, out []byte) error {
			var recs []struct {
				ID string `json:"id"`
			}
			if err := decodeJSON(out, &recs); err != nil {
				return err
			}
			if len(recs) != 1 || recs[0].ID == "" {
				return fmt.Errorf("the import printed %d records, want 1", len(recs))
			}
			s.refs[f.name] = recs[0].ID
			return nil
		},
	}
}

// wantNew requires at least one record of the step that matches keep.
func wantNew(s *state, what string, keep func(manifestRec) bool) ([]manifestRec, error) {
	added, err := s.added()
	if err != nil {
		return nil, err
	}
	var hit []manifestRec
	for _, r := range added {
		if keep(r) {
			hit = append(hit, r)
		}
	}
	if len(hit) == 0 {
		return nil, fmt.Errorf("no new artifact is %s (%d new in all)", what, len(added))
	}
	return hit, nil
}

// imageStep is `image <verb...> --case C <fixture ref> <extra...>` expecting exit 0 and a new artifact.
func imageStep(name, fix string, verb []string, extra []string, what string, keep func(manifestRec) bool) step {
	return step{
		name: name,
		args: func(s *state) ([]string, error) {
			args := append([]string{"image"}, verb...)
			args = append(args, "--case", s.caseDir, s.refs[fix])
			return append(args, extra...), nil
		},
		check: func(s *state, _ []byte) error {
			_, err := wantNew(s, what, keep)
			return err
		},
	}
}

// carvedSQLiteSHA is the sha256 of the one deleted, validated sqlite object of the carve-fat12 oracle.
func carvedSQLiteSHA(raw []byte) (string, error) {
	var o struct {
		Images []struct {
			Name    string `json:"name"`
			Objects []struct {
				Type   string `json:"type"`
				SHA256 string `json:"sha256"`
				State  string `json:"state"`
				Expect string `json:"expect"`
			} `json:"objects"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", err
	}
	var found []string
	for _, im := range o.Images {
		if im.Name != "carve-fat12" {
			continue
		}
		for _, ob := range im.Objects {
			if ob.Type == "sqlite" && ob.State == "deleted" && ob.Expect == "validated" && ob.SHA256 != "" {
				found = append(found, ob.SHA256)
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("the carve oracle has %d deleted, validated sqlite objects in carve-fat12, want 1", len(found))
	}
	return found[0], nil
}

// firstLiveSeq is the sequence number of the first committed transaction of the live region in the journal oracle.
func firstLiveSeq(raw []byte) (int64, error) {
	var o struct {
		Txns []struct {
			Seq       int64  `json:"seq"`
			Region    string `json:"region"`
			Committed bool   `json:"committed"`
		} `json:"txns"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return 0, err
	}
	for _, t := range o.Txns {
		if t.Committed && t.Region == "live" {
			return t.Seq, nil
		}
	}
	return 0, errors.New("the journal oracle has no committed transaction in the live region")
}

type sqliteReport struct {
	Inputs []struct {
		Role          string `json:"role"`
		Recovered     bool   `json:"recovered"`
		ConfidenceCap *int   `json:"confidence_cap"`
	} `json:"inputs"`
	Counts struct {
		Written int64 `json:"written"`
	} `json:"counts"`
}

// maxRecoveredCap is the highest confidence cap the recovered database may carry into its rows.
const maxRecoveredCap = 70

func checkSQLiteReport(s *state, out []byte) error {
	var rep sqliteReport
	if err := decodeJSON(out, &rep); err != nil {
		return err
	}
	for _, in := range rep.Inputs {
		if in.Role != "db" {
			continue
		}
		if !in.Recovered {
			return errors.New("the database input is not marked recovered")
		}
		if in.ConfidenceCap == nil || *in.ConfidenceCap > maxRecoveredCap {
			return fmt.Errorf("the database input's confidence cap is %v, want at most %d", in.ConfidenceCap, maxRecoveredCap)
		}
		fmt.Fprintf(s.out, "     sqlite recover: counts.written %d, confidence cap %d\n", rep.Counts.Written, *in.ConfidenceCap)
		return nil
	}
	return errors.New("the report has no database input")
}

func noProblem(s *state, out []byte) error {
	for _, b := range [][]byte{out, s.stderr} {
		if strings.Contains(string(b), "PROBLEM:") {
			return errors.New("case verify reports a problem: " + tail(b))
		}
	}
	return nil
}

// tamper flips byte 0 of the first deleted-file artifact's file in the case.
func tamper(s *state) error {
	recs, err := readManifest(s.caseDir)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Class != "deleted-file" {
			continue
		}
		p := filepath.Join(s.caseDir, filepath.FromSlash(r.Path))
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // inside the tool's scratch case
		if err != nil {
			return err
		}
		if len(b) == 0 {
			continue
		}
		b[0] ^= 0xff
		return os.WriteFile(p, b, 0o600) //nolint:gosec // inside the tool's scratch case
	}
	return errors.New("no non-empty deleted-file artifact to tamper with")
}

func isMethod(class string, methods ...string) func(manifestRec) bool {
	return func(r manifestRec) bool {
		return r.Class == class && (len(methods) == 0 || slices.Contains(methods, r.Method))
	}
}

// steps is the fixed table (3K-R6, R9). Each wantExit comes from the fixture's oracle: a recovery step is 1 only
// when its oracle has totals.undecided > 0, and none of these has.
func steps() []step {
	var t []step
	t = append(t, step{
		name: "case-new",
		args: func(s *state) ([]string, error) {
			return []string{"case", "new", "--dir", s.scratch, "--id", "ACCEPT", "--examiner", "acceptance"}, nil
		},
		check: func(s *state, _ []byte) error {
			// the manifest is created by the first artifact, so the case is recognised by its own files
			for _, f := range []string{"case.json", "audit.jsonl"} {
				if _, err := os.Stat(filepath.Join(s.caseDir, f)); err != nil {
					return err
				}
			}
			return nil
		},
	})
	for _, f := range fixtures {
		t = append(t, importStep(f))
	}
	t = append(t,
		imageStep("recover-fat", "fat16-recover", []string{"recover"}, []string{"--all", "--json"},
			"a deleted-file recovered by fat-contiguous", isMethod("deleted-file", "fat-contiguous")),
		imageStep("recover-ext4", "ext4-recover", []string{"recover"}, []string{"--all", "--json"},
			"a deleted-file recovered by ext-inode-intact or ext4-extent-leaf", isMethod("deleted-file", "ext-inode-intact", "ext4-extent-leaf")),
		imageStep("recover-f2fs", "f2fs-recover-nodiscard", []string{"recover"}, []string{"--all", "--json"},
			"a recovery by f2fs-node-scan", func(r manifestRec) bool { return r.Method == "f2fs-node-scan" }),
		step{
			name: "carve",
			args: func(s *state) ([]string, error) {
				return []string{"image", "carve", "--case", s.caseDir, s.refs["carve-fat12"], "--json"}, nil
			},
			check: func(s *state, _ []byte) error {
				want, err := carvedSQLiteSHA(s.oracle["carve-fat12"])
				if err != nil {
					return err
				}
				hit, err := wantNew(s, "a carved artifact", isMethod("carved"))
				if err != nil {
					return err
				}
				for _, r := range hit {
					if r.Kind == "carve" && r.SHA256 == want {
						s.refs["sqlite"] = r.ID
						return nil
					}
				}
				return fmt.Errorf("no carved artifact has the oracle's sqlite object hash %s", want)
			},
		},
		step{
			name: "sqlite-recover",
			args: func(s *state) ([]string, error) {
				return []string{"sqlite", "recover", "--case", s.caseDir, "--json", s.refs["sqlite"]}, nil
			},
			check: checkSQLiteReport,
		},
		imageStep("slack", "fat16-slack", []string{"slack"}, []string{"--json"},
			"a slack artifact", func(r manifestRec) bool { return r.Kind == "slack" }),
		imageStep("ext4-journal-report", "ext4-journal-crash", []string{"journal", "report"}, nil,
			"a journal-report", isMethod("journal-report")),
		step{
			name: "ext4-journal-blocks",
			args: func(s *state) ([]string, error) {
				seq, err := firstLiveSeq(s.oracle["ext4-journal-crash"])
				if err != nil {
					return nil, err
				}
				return []string{"image", "journal", "blocks", "--case", s.caseDir, s.refs["ext4-journal-crash"], fmt.Sprint(seq), "--region", "live"}, nil
			},
			check: func(s *state, _ []byte) error {
				_, err := wantNew(s, "a journal-block", isMethod("journal-block"))
				return err
			},
		},
		imageStep("ext4-use-journal", "ext4-journal-crash", []string{"recover"}, []string{"--all", "--use-journal", "stale", "--json"},
			"a recovery by ext4-journal-inode", func(r manifestRec) bool { return r.Method == "ext4-journal-inode" }),
		imageStep("f2fs-journal-report", "f2fs-rollforward", []string{"journal", "report"}, nil,
			"a journal-report", isMethod("journal-report")),
		imageStep("f2fs-rollforward", "f2fs-rollforward", []string{"recover"}, []string{"--rollforward", "--all", "--json"},
			"a post-checkpoint-file", isMethod("post-checkpoint-file")),
		step{
			name:  "verify",
			args:  func(s *state) ([]string, error) { return []string{"case", "verify", "--case", s.caseDir}, nil },
			check: noProblem,
		},
		step{
			name: "verify-tamper",
			args: func(s *state) ([]string, error) {
				if err := tamper(s); err != nil {
					return nil, err
				}
				return []string{"case", "verify", "--case", s.caseDir}, nil
			},
			wantExit: 4,
		},
	)
	return t
}
