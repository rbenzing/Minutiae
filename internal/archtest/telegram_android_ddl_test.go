package archtest

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Telegram Android cache4.db DDL snapshots of plan 4I4 (Task 4; spec §6.1-§6.3, §13 first bullet, 4I4-P3 a).
// One directory holds the snapshots of four pins; each names its tag, the commit of the pin whose sources its run
// checked, its verdict and the user version its fixtures carry. v85 has no snapshot (spec §13: no sampled tag has
// 85; media_v4 is covered by v159.sql).

// telegramDDLSnapshots maps each snapshot to the tag, the pin (a key of telegramPins), the user version and the
// verdict it names.
var telegramDDLSnapshots = map[string]struct {
	tag, pin, version, verdict string
}{
	"v181.sql":              {"master", "A-HEAD", "181", "verified"},
	"v159.sql":              {"release-11.4.2-5469", "A-11.4.2", "159", "verified"},
	"v84.sql":               {"release-8.1.1_2431", "release-8.1.1_2431", "84", "verified"},
	"v64.sql":               {"release-5.14.0_1851", "release-5.14.0_1851", "64", "verified"},
	"v84-upgraded-v181.sql": {"master", "A-HEAD", "181", "historical"},
}

var (
	tgDDLTagRE     = regexp.MustCompile(`(?m)^-- tag (\S+)$`)
	tgDDLCommitRE  = regexp.MustCompile(`(?m)^-- commit ([0-9a-f]{40})$`)
	tgDDLVerdictRE = regexp.MustCompile(`(?m)^-- verdict (verified|historical)$`)
	tgDDLVersionRE = regexp.MustCompile(`(?m)^-- user_version ([0-9]+)$`)
)

// tgDDLColumns returns the column names of a table of a snapshot (table constraints left out), whether the table
// is there, and its table constraint text (the PRIMARY KEY clause) when it has one.
func tgDDLColumns(src, table string) (cols []string, ok bool, pk string) {
	m := regexp.MustCompile(`(?m)^CREATE TABLE ` + table + ` \((.*)\);$`).FindStringSubmatch(src)
	if m == nil {
		return nil, false, ""
	}
	depth, cur := 0, ""
	var items []string
	for _, c := range m[1] {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		}
		if c == ',' && depth == 0 {
			items = append(items, strings.TrimSpace(cur))
			cur = ""
			continue
		}
		cur += string(c)
	}
	items = append(items, strings.TrimSpace(cur))
	for _, it := range items {
		if strings.HasPrefix(it, "PRIMARY KEY") {
			pk = it
			continue
		}
		cols = append(cols, strings.Fields(it)[0])
	}
	return cols, true, pk
}

// TestTelegramDDLSnapshotsCiteTheirSource pins the provenance of the five snapshots (spec §13 first bullet,
// 4I4-P3 a): exactly the five files; each with one `-- tag`, one `-- commit` of a Task 1 pin, one `-- verdict`, one
// `-- user_version` and the tables-only sentence; CROSSCHECK.md holds for every CREATE TABLE of every snapshot a
// row with the snapshot's verdict (or MANUAL, read by hand), no MISMATCH, and no URL; every file loads in the engine.
func TestTelegramDDLSnapshotsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "telegram-android")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var names, want []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	for n := range telegramDDLSnapshots {
		want = append(want, n)
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("snapshots %v, want exactly %v", names, want)
	}
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))
	if strings.Contains(report, "MISMATCH") {
		t.Error("CROSSCHECK.md lists a MISMATCH: fix the snapshot, never the report")
	}
	if !strings.Contains(report, "\n# DDL cross-check (plan 4I4)\n") {
		t.Error("CROSSCHECK.md has no section \"# DDL cross-check (plan 4I4)\"")
	}
	for _, line := range calendarURLLines(report) {
		t.Errorf("CROSSCHECK.md cites a URL: %q", line)
	}
	for _, base := range names {
		exp := telegramDDLSnapshots[base]
		src := calendarReadFile(t, filepath.Join(dir, base))
		for _, line := range calendarURLLines(src) {
			t.Errorf("%s cites a URL: %q", base, line)
		}
		for name, re := range map[string]*regexp.Regexp{"tag": tgDDLTagRE, "commit": tgDDLCommitRE, "verdict": tgDDLVerdictRE, "user_version": tgDDLVersionRE} {
			anyLine := regexp.MustCompile(`(?m)^-- ` + name + ` `)
			if len(re.FindAllString(src, -1)) != 1 || len(anyLine.FindAllString(src, -1)) != 1 {
				t.Errorf("%s: want exactly one well-formed '-- %s' line", base, name)
			}
		}
		if m := tgDDLTagRE.FindStringSubmatch(src); m == nil || m[1] != exp.tag {
			t.Errorf("%s: tag %v, want %s", base, m, exp.tag)
		}
		if m := tgDDLCommitRE.FindStringSubmatch(src); m == nil || m[1] != telegramPins[exp.pin] {
			t.Errorf("%s: commit %v, want the pin %s commit %s", base, m, exp.pin, telegramPins[exp.pin])
		}
		if m := tgDDLVerdictRE.FindStringSubmatch(src); m == nil || m[1] != exp.verdict {
			t.Errorf("%s: verdict %v, want %s", base, m, exp.verdict)
		}
		if m := tgDDLVersionRE.FindStringSubmatch(src); m == nil || m[1] != exp.version {
			t.Errorf("%s: user_version %v, want %s", base, m, exp.version)
		}
		if !strings.Contains(src, "tables only; indexes and triggers are not part of the snapshot") {
			t.Errorf("%s lacks the tables-only sentence", base)
		}
		tables := calendarCreateTables(src)
		if len(tables) == 0 {
			t.Errorf("%s has no CREATE TABLE", base)
		}
		for _, tb := range tables {
			got, ok := calendarCrosscheckVerdict(report, base, tb)
			switch {
			case !ok:
				t.Errorf("CROSSCHECK.md has no row for %s table %s", base, tb)
			case got == exp.verdict:
			case exp.verdict == "verified" && strings.HasPrefix(got, "MANUAL, read by hand at "):
			default:
				t.Errorf("%s says verdict %s but CROSSCHECK.md says %q for table %s", base, exp.verdict, got, tb)
			}
		}
	}
	t.Run("LoadInTheEngine", func(t *testing.T) {
		for _, base := range names {
			src := calendarReadFile(t, filepath.Join(dir, base))
			if n, want := calendarLoadInEngine(t, src), len(calendarCreateTables(src)); n != want {
				t.Errorf("%s: the engine holds %d tables, the snapshot creates %d", base, n, want)
			}
		}
	})
}

// TestTelegramDDLSnapshotColumns pins the columns later tasks rely on (spec §6.3, recon §1.4): v181's
// messages_v2 is exactly the twenty columns of spec §6.3 in order, keyed (mid, uid); enc_chats has the 19 columns of
// recon §1.4 (the parser names only two of them, spec §9); v64 has messages with mid INTEGER PRIMARY KEY (the
// columns of facts.tsv columns.messages.v64) and no messages_v2.
func TestTelegramDDLSnapshotColumns(t *testing.T) {
	dir := calendarSchemaDir(t, "telegram-android")
	v181 := calendarReadFile(t, filepath.Join(dir, "v181.sql"))
	cols, ok, pk := tgDDLColumns(v181, "messages_v2")
	wantCols := strings.Fields("mid uid read_state send_state date data out ttl media replydata imp mention forwards " +
		"replies_data thread_reply_id is_channel reply_to_message_id custom_params group_id reply_to_story_id")
	if !ok || !slices.Equal(cols, wantCols) || len(cols) != 20 {
		t.Errorf("v181 messages_v2 columns %v, want the twenty of spec §6.3 %v", cols, wantCols)
	}
	if pk != "PRIMARY KEY(mid, uid)" {
		t.Errorf("v181 messages_v2 constraint %q, want PRIMARY KEY(mid, uid)", pk)
	}
	enc, ok, _ := tgDDLColumns(v181, "enc_chats")
	wantEnc := strings.Fields("uid user name data g authkey ttl layer seq_in seq_out use_count exchange_id key_date fprint " +
		"fauthkey khash in_seq_no admin_id mtproto_seq")
	if !ok || !slices.Equal(enc, wantEnc) || len(enc) != 19 {
		t.Errorf("v181 enc_chats columns %v, want the 19 of recon §1.4", enc)
	}

	v64 := calendarReadFile(t, filepath.Join(dir, "v64.sql"))
	if _, has, _ := tgDDLColumns(v64, "messages_v2"); has {
		t.Error("v64 has messages_v2; release-5.14.0_1851 predates it")
	}
	cols64, has, _ := tgDDLColumns(v64, "messages")
	if !has {
		t.Fatal("v64 has no messages table")
	}
	if !strings.Contains(v64, "CREATE TABLE messages (mid INTEGER PRIMARY KEY, ") {
		t.Error("v64 messages does not start with mid INTEGER PRIMARY KEY")
	}
	var fact string
	for _, line := range strings.Split(calendarReadFile(t, filepath.Join(dir, "facts.tsv")), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 5 && f[0] == "columns.messages.v64" {
			fact = f[1]
		}
	}
	if fact == "" || strings.Join(cols64, ",") != fact {
		t.Errorf("v64 messages columns %v, facts.tsv columns.messages.v64 is %q", cols64, fact)
	}
}
