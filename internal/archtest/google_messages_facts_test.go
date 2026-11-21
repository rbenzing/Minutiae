package archtest

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Google Messages facts and bugle_db DDL snapshots of plan 4I5 (Task 1; spec §5.3, §6.1-§6.3, §10 first three
// bullets). The repository holds facts only (codes, constant names, column lists, line pins) read from the AOSP
// Messaging sources; no source text is copied.

const gmFactsHeader = "key\tvalue\tconstant\tcontract\tpin\tsource\tnote"

// gmMessageProtocol is spec §6.1: the channel each AOSP message_protocol value maps to.
var gmMessageProtocol = map[string]string{"-1": "unknown", "0": "sms", "1": "mms", "2": "mms"}

// gmMessageStatus is spec §6.2: "<direction>/<status>" of each AOSP message_status value.
var gmMessageStatus = map[string]string{
	"0": "unknown/unknown", "1": "out/sent", "2": "out/delivered", "3": "out/draft",
	"4": "out/queued", "5": "out/queued", "6": "out/queued", "7": "out/queued",
	"8": "out/failed", "9": "out/failed",
	"100": "in/received", "101": "in/unknown", "102": "in/unknown", "103": "in/unknown", "104": "in/unknown",
	"105": "in/unknown", "106": "in/unknown", "107": "in/unknown",
}

// gmOtherKeys are the facts.tsv keys that are not code rows.
var gmOtherKeys = strings.Fields("self.other_than_self_sub_id self.is_self self.default_sub_id self.destinations_null " +
	"members.exclude_self contact_id.unresolved time.unit time.sentinel trigger.parts_timestamp_insert " +
	"trigger.parts_timestamp_update messages.id_autoincrement rebuild.drop_recreate version.current version.upgrade_1_2 " +
	"uri.sms uri.mms uri.written uri.mms_part sms_id.autoincrement pdu_id.autoincrement parts.text_or_uri " +
	"parts.dims_unspecified read.values sync.status_mapping delete.row_removal blocked.archives path.package_aosp path.database")

var (
	gmSourceRE   = regexp.MustCompile(`^[A-Za-z]+\.(java|xml):[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)
	gmConstantRE = regexp.MustCompile(`^(PROTOCOL|BUGLE_STATUS)_[A-Z_]+$`)
	gmCommitRE   = regexp.MustCompile(`(?m)^-- commit ([0-9a-f]{40})$`)
	gmVerdictRE  = regexp.MustCompile(`(?m)^-- verdict (\S+)$`)
	gmVersionRE  = regexp.MustCompile(`(?m)^-- user_version ([0-9]+)$`)
)

const gmCommit = "de315b762312dd1a5d2bbd16e62ef2bd123f61e5"

// gmColumns returns the columns of a table of a snapshot in order (table constraints left out), and whether the
// table is there. A constraint item starts with a keyword; a run of FOREIGN KEY clauses is one item.
func gmColumns(src, table string) ([]string, bool) {
	m := regexp.MustCompile(`(?m)^CREATE TABLE ` + table + ` \((.*)\);$`).FindStringSubmatch(src)
	if m == nil {
		return nil, false
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
	var cols []string
	for _, it := range items {
		w := strings.Fields(it)[0]
		if slices.Contains([]string{"PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT"}, strings.ToUpper(w)) {
			continue
		}
		cols = append(cols, w)
	}
	return cols, true
}

// gmFactRows reads facts.tsv into key -> fields (7 fields each), reporting a malformed row or a repeated key.
func gmFactRows(t *testing.T, src string) map[string][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	if lines[0] != gmFactsHeader {
		t.Fatalf("facts.tsv header %q, want %q", lines[0], gmFactsHeader)
	}
	rows := map[string][]string{}
	for _, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			t.Errorf("facts.tsv row has %d fields, want 7: %q", len(f), line)
			continue
		}
		if _, dup := rows[f[0]]; dup {
			t.Errorf("facts.tsv repeats the key %s", f[0])
		}
		rows[f[0]] = f
	}
	return rows
}

// TestGoogleMessagesFactsCiteTheirSource pins the provenance of facts.tsv (spec §5.3, §6.1-§6.2, C8): the header, 7
// fields per row, unique keys, a pin and a file:line source on every row, the code rows equal to spec §6.1-§6.2, and a
// CROSSCHECK.md row that says how each key was checked; no URL anywhere.
func TestGoogleMessagesFactsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "google-messages")
	facts := calendarReadFile(t, filepath.Join(dir, "facts.tsv"))
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))
	rows := gmFactRows(t, facts)

	want := slices.Clone(gmOtherKeys)
	for v := range gmMessageProtocol {
		want = append(want, "message_protocol."+v)
	}
	for v := range gmMessageStatus {
		want = append(want, "message_status."+v)
	}
	for _, k := range want {
		if _, ok := rows[k]; !ok {
			t.Errorf("facts.tsv lacks the key %s", k)
		}
	}
	for k, f := range rows {
		if !slices.Contains(want, k) {
			t.Errorf("facts.tsv holds the unexpected key %s", k)
		}
		if f[1] == "" {
			t.Errorf("%s has an empty value", k)
		}
		if !slices.Contains([]string{"M", "T10", "F10"}, f[4]) {
			t.Errorf("%s: pin %q, want M, T10 or F10", k, f[4])
		}
		if !gmSourceRE.MatchString(f[5]) {
			t.Errorf("%s: source %q is not <File>.<ext>:<line>[-<line>]...", k, f[5])
		}
		row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(k) + ` \| (verified at|MANUAL, read by hand at) `)
		if !row.MatchString(report) {
			t.Errorf("CROSSCHECK.md has no 'verified at' or 'MANUAL, read by hand at' row for %s", k)
		}
	}
	codes := func(prefix string, contracts map[string]string) {
		for v, contract := range contracts {
			f, ok := rows[prefix+"."+v]
			if !ok {
				continue
			}
			if f[3] != contract {
				t.Errorf("%s.%s contract %q, want %q (spec §6.1-§6.2)", prefix, v, f[3], contract)
			}
			if !gmConstantRE.MatchString(f[2]) {
				t.Errorf("%s.%s constant %q does not match %s", prefix, v, f[2], gmConstantRE)
			}
		}
	}
	codes("message_protocol", gmMessageProtocol)
	codes("message_status", gmMessageStatus)
	if f := rows["self.other_than_self_sub_id"]; f != nil && f[1] != "-2" {
		t.Errorf("self.other_than_self_sub_id is %q, want -2", f[1])
	}
	if strings.Contains(report, "MISMATCH") {
		t.Error("CROSSCHECK.md lists a MISMATCH: fix the snapshot or the fact, never the report")
	}
	for _, name := range []string{"facts.tsv", "CROSSCHECK.md", "aosp-v2.sql", "aosp-v1.sql", "proprietary-shaped.sql"} {
		for _, line := range calendarURLLines(calendarReadFile(t, filepath.Join(dir, name))) {
			t.Errorf("%s cites a URL: %q", name, line)
		}
	}
}

// TestGoogleMessagesDDLSnapshotsCiteTheirSource pins the two AOSP snapshots (spec §6.3, §10, recon §2.1): commit,
// distinct verdicts and versions 2 and 1, the five tables, the columns of messages and parts, v1 equal to v2 minus
// IS_ENTERPRISE, the two triggers, CROSSCHECK.md rows without MISMATCH, and every file loads in the engine.
func TestGoogleMessagesDDLSnapshotsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "google-messages")
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))
	if strings.Contains(report, "MISMATCH") {
		t.Error("CROSSCHECK.md lists a MISMATCH: fix the snapshot, never the report")
	}
	wantTables := []string{"conversations", "messages", "parts", "participants", "conversation_participants"}
	snaps := map[string]string{}
	verdicts := map[string]string{}
	for base, ver := range map[string]string{"aosp-v2.sql": "2", "aosp-v1.sql": "1"} {
		src := calendarReadFile(t, filepath.Join(dir, base))
		snaps[base] = src
		for name, re := range map[string]*regexp.Regexp{"commit": gmCommitRE, "verdict": gmVerdictRE, "user_version": gmVersionRE} {
			if len(re.FindAllString(src, -1)) != 1 || len(regexp.MustCompile(`(?m)^-- `+name+` `).FindAllString(src, -1)) != 1 {
				t.Errorf("%s: want exactly one well-formed '-- %s' line", base, name)
			}
		}
		if m := gmCommitRE.FindStringSubmatch(src); m == nil || m[1] != gmCommit {
			t.Errorf("%s: commit %v, want %s", base, m, gmCommit)
		}
		m := gmVerdictRE.FindStringSubmatch(src)
		if m == nil || (m[1] != "verified" && m[1] != "historical") {
			t.Fatalf("%s: verdict %v, want verified or historical", base, m)
		}
		verdicts[base] = m[1]
		if m := gmVersionRE.FindStringSubmatch(src); m == nil || m[1] != ver {
			t.Errorf("%s: user_version %v, want %s", base, m, ver)
		}
		tables := calendarCreateTables(src)
		if !slices.Equal(tables, wantTables) {
			t.Errorf("%s creates %v, want %v", base, tables, wantTables)
		}
		for _, tb := range tables {
			got, ok := calendarCrosscheckVerdict(report, base, tb)
			switch {
			case !ok:
				t.Errorf("CROSSCHECK.md has no row for %s table %s", base, tb)
			case got == verdicts[base]:
			case strings.HasPrefix(got, "MANUAL, read by hand at DatabaseHelper.java:"):
			default:
				t.Errorf("%s says verdict %s but CROSSCHECK.md says %q for table %s", base, verdicts[base], got, tb)
			}
		}
		triggers := regexp.MustCompile(`(?m)^CREATE TRIGGER .*$`).FindAllString(src, -1)
		if len(triggers) != 2 {
			t.Errorf("%s holds %d CREATE TRIGGER lines, want 2", base, len(triggers))
		}
		for _, tr := range triggers {
			if !strings.Contains(tr, " parts") || !strings.Contains(tr, "timestamp") {
				t.Errorf("%s: trigger does not name parts and timestamp: %q", base, tr)
			}
		}
	}
	if verdicts["aosp-v2.sql"] == verdicts["aosp-v1.sql"] {
		t.Errorf("both snapshots say verdict %s; exactly one is the tag's own text (spec §10)", verdicts["aosp-v2.sql"])
	}

	v2, v1 := snaps["aosp-v2.sql"], snaps["aosp-v1.sql"]
	msg, _ := gmColumns(v2, "messages")
	wantMsg := strings.Fields("_id conversation_id sender_id sent_timestamp received_timestamp message_protocol message_status seen read " +
		"sms_message_uri sms_priority sms_message_size mms_subject mms_transaction_id mms_content_location mms_expiry raw_status self_id retry_start_timestamp")
	if !slices.Equal(msg, wantMsg) || len(msg) != 19 {
		t.Errorf("messages columns %v, want the 19 of recon §2.1 %v", msg, wantMsg)
	}
	if !strings.Contains(v2, "CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, ") {
		t.Error("messages does not declare _id INTEGER PRIMARY KEY AUTOINCREMENT")
	}
	parts, _ := gmColumns(v2, "parts")
	wantParts := strings.Fields("_id message_id text uri content_type width height timestamp conversation_id")
	if !slices.Equal(parts, wantParts) {
		t.Errorf("parts columns %v, want the 9 of recon §2.1 %v", parts, wantParts)
	}
	c2, _ := gmColumns(v2, "conversations")
	c1, _ := gmColumns(v1, "conversations")
	if i := slices.Index(c2, "IS_ENTERPRISE"); i < 0 || !slices.Equal(slices.Delete(slices.Clone(c2), i, i+1), c1) {
		t.Errorf("v1 conversations %v is not v2 %v minus IS_ENTERPRISE", c1, c2)
	}
	for _, tb := range wantTables[1:] {
		line := regexp.MustCompile(`(?m)^CREATE TABLE ` + tb + ` .*$`)
		if line.FindString(v1) != line.FindString(v2) {
			t.Errorf("%s: the v1 statement differs from the v2 one", tb)
		}
	}
	t.Run("LoadInTheEngine", func(t *testing.T) {
		for base, src := range snaps {
			if n, want := calendarLoadInEngine(t, src), len(calendarCreateTables(src)); n != want {
				t.Errorf("%s: the engine holds %d tables, the snapshot creates %d", base, n, want)
			}
		}
	})
}

// TestGoogleMessagesProprietaryShapeIsMarkedFromMemory pins proprietary-shaped.sql (spec §10, §15.6): marked
// from-memory with user_version 9000, the AOSP v2 tables plus exactly the five named columns, and no RCS, reaction or
// trash name (those are unverified and must not be invented).
func TestGoogleMessagesProprietaryShapeIsMarkedFromMemory(t *testing.T) {
	dir := calendarSchemaDir(t, "google-messages")
	src := calendarReadFile(t, filepath.Join(dir, "proprietary-shaped.sql"))
	v2 := calendarReadFile(t, filepath.Join(dir, "aosp-v2.sql"))
	if m := gmVerdictRE.FindStringSubmatch(src); m == nil || m[1] != "from-memory" {
		t.Errorf("verdict %v, want from-memory", m)
	}
	if m := gmVersionRE.FindStringSubmatch(src); m == nil || m[1] != "9000" {
		t.Errorf("user_version %v, want 9000", m)
	}
	extra := map[string][]string{
		"parts":         {"file_size_bytes", "local_cache_path", "fixture_extra_part"},
		"messages":      {"fixture_extra_flag"},
		"conversations": {"fixture_extra_note"},
	}
	tables := calendarCreateTables(src)
	if !slices.Equal(tables, calendarCreateTables(v2)) {
		t.Errorf("tables %v, want those of aosp-v2.sql %v", tables, calendarCreateTables(v2))
	}
	for _, tb := range tables {
		got, _ := gmColumns(src, tb)
		base, _ := gmColumns(v2, tb)
		if want := append(slices.Clone(base), extra[tb]...); !slices.Equal(got, want) {
			t.Errorf("%s columns %v, want the v2 columns plus the extras: %v", tb, got, want)
		}
		for _, name := range append([]string{tb}, got...) {
			low := strings.ToLower(name)
			for _, bad := range []string{"rcs", "reaction", "trash"} {
				if strings.Contains(low, bad) {
					t.Errorf("%s.%s holds %q: no unverified feature may be invented", tb, name, bad)
				}
			}
		}
	}
	if n := calendarLoadInEngine(t, src); n != len(tables) {
		t.Errorf("the engine holds %d tables, the snapshot creates %d", n, len(tables))
	}
}
