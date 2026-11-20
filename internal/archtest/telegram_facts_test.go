package archtest

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Telegram Android TL facts of plan 4I4 (Task 1; spec §6, §13 second bullet, §17 Q1-Q3, rulings T2, T3,
// T5, T6; writer's calls 4I4-P3 b and 4I4-P4; preflight F25). The constructor table and the other pinned facts
// are data files under tools/fixtures/schemas/telegram-android, each row citing <File>.java:<line> at one of four
// pinned commits of the app source; CROSSCHECK.md holds the script's report of every row.

const (
	telegramConstructorsHeader = "type\tid\tclass\tbody\tpin\tsource\tfields"
	telegramFactsHeader        = "key\tvalue\tpin\tsource\tnote"
)

// telegramPins are the four pins of 4I4-P4 and the commit each names (the three tags peeled with ls-remote).
var telegramPins = map[string]string{
	"A-HEAD":              "21f7d109da6ab3506c9e9577f98c6a38771ab335",
	"A-11.4.2":            "fb2e545101f41303f1e2712de2e7611a9335f1c3",
	"release-8.1.1_2431":  "58aded63e9713ef796d76f3e29a60a5254f803b4",
	"release-5.14.0_1851": "7d6fa267c13bacd48f8d8f10d31a61f38b77bfe4",
}

// telegramTypeCounts are the rows per TL type (spec §6.6, T3, Task 1). Message: the 46 classes A-HEAD reads, the
// alias 0xa4e97f37, and two IDs only the older pins read (TL_message_layer104_2, _3, pinned at A-11.4.2); Chat: 37
// plus two A-11.4.2 IDs (TL_channel_layer167_2, _3); PhoneCallDiscardReason: 5 plus the release-5.14.0_1851
// AllowGroupCall; MessageMedia: 50 TLRPC classes plus the two app-local story classes the A-HEAD switch reads
// (0xc79aee1d, 0xc79aee1f, opaque); MessageReplyHeader and MessageEntity: every constructor A-HEAD reads (T3).
var telegramTypeCounts = map[string]int{
	"Message": 49, "Peer": 6, "MessageFwdHeader": 7, "MessageAction": 1, "PhoneCallDiscardReason": 6,
	"MessageMedia": 52, "User": 29, "Chat": 39, "GeoPoint": 4, "MessageReplyHeader": 9, "MessageEntity": 28,
	"Photo": 8, "Document": 10, "WebPage": 12,
}

// telegramRequiredIDs are IDs every later task relies on, with the class the recon names (recon §3.2-§3.6).
var telegramRequiredIDs = map[string]string{
	"0x80e11a7f": "TL_messageActionPhoneCall",
	"0x85e42301": "TL_phoneCallDiscardReasonMissed",
	"0xfaf7e8c9": "TL_phoneCallDiscardReasonBusy",
	"0x57adc690": "TL_phoneCallDiscardReasonHangup",
	"0xe095c1a0": "TL_phoneCallDiscardReasonDisconnect",
	"0x9fbbf1f7": "TL_phoneCallDiscardReasonMigrateConferenceCall",
	"0x555555fa": "TL_message_secret",
	"0x555555f9": "TL_message_secret_layer72",
	"0x555555f8": "TL_message_secret_old",
	"0x7600b9d3": "TL_message",
	"0x7a800e0a": "TL_messageService",
	"0x90a6ca84": "TL_messageEmpty",
	"0xa4e97f37": "TL_message_layer179",
	"0x59511722": "TL_peerUser",
	"0x36c6019a": "TL_peerChat",
	"0xa2a5371e": "TL_peerChannel",
	"0x9db1bc6d": "TL_peerUser_layer131",
	"0xbad0e5bb": "TL_peerChat_layer131",
	"0xbddde532": "TL_peerChannel_layer131",
	"0x55555558": "TL_documentEncrypted",
}

// telegramFactKeys are the fixed facts.tsv keys (Task 1), each with the pins it must be stated at.
var telegramFactKeys = map[string][]string{
	"write_verb.messages_v2.put":  {"A-HEAD", "A-11.4.2", "release-8.1.1_2431"},
	"write_verb.messages_v2.edit": {"A-HEAD", "A-11.4.2", "release-8.1.1_2431"},
	"write_verb.messages.put":     {"release-5.14.0_1851"},
	"write_verb.messages.edit":    {"release-5.14.0_1851"},
	"unit.call_duration":          {"A-HEAD"},
	"unit.edit_date":              {"A-HEAD"},
	"unit.blob_times":             {"A-HEAD"},
	"predicate.pre84_secret_uid":  {"A-HEAD"},
	"predicate.pre84_channel_mid": {"A-HEAD"},
	"predicate.secret_dialog":     {"A-HEAD"},
	"predicate.folder_dialog":     {"A-HEAD"},
	"cast.encrypted_chat_id":      {"A-HEAD"},
	"line.v85_write":              {"A-HEAD"},
	"columns.messages.v64":        {"release-5.14.0_1851"},
}

var (
	tgIDRE     = regexp.MustCompile(`^0x[0-9a-f]{8}$`)
	tgSourceRE = regexp.MustCompile(`^[A-Za-z_]+\.java:[0-9]+$`)
	// A facts source may cite a line range (a predicate spans lines).
	tgFactSourceRE = regexp.MustCompile(`^[A-Za-z_]+\.java:[0-9]+(-[0-9]+)?$`)
	// The 4I4-P4 grammar, one token at a time: a gate opens with "[F.n:" (or "[F2.n:"), holds one or more
	// fields and closes on the "]" that ends its last field; gates do not nest.
	tgGateOpenRE = regexp.MustCompile(`^\[F2?\.[0-9]+:$`)
	tgName       = `[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`
	tgType       = `[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`
	tgFieldRE    = regexp.MustCompile(`^(` + tgName + `)(?:=F2?\.[0-9]+|:(#|i32|i64|dbl|bool|bytes|str|secret|` +
		`(id32|id64)@(user|chat|channel|unknown)|vec<i32>|vec<i64>|vec<` + tgType + `>|` + tgType + `))(\]?)$`)
	tgWrapperValues = []string{"user", "chat", "channel", "unknown"}
)

// tgField is one field token of a constructors.tsv row: its name, kind (empty for a flag-only true) and, for an
// inline id, the resolved class.
type tgField struct{ name, kind, class string }

// tgSHA256 is the lowercase hex SHA-256 of a file's text as read.
func tgSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// tgParseFields checks the fields column against the 4I4-P4 grammar and returns its fields.
func tgParseFields(t *testing.T, row string, s string) []tgField {
	t.Helper()
	var out []tgField
	inGate, gateLen := false, 0
	for tok := range strings.FieldsSeq(s) {
		if tgGateOpenRE.MatchString(tok) {
			if inGate {
				t.Errorf("%s: a gate opens inside a gate at %q", row, tok)
			}
			inGate, gateLen = true, 0
			continue
		}
		m := tgFieldRE.FindStringSubmatch(tok)
		if m == nil {
			t.Errorf("%s: token %q is outside the 4I4-P4 grammar", row, tok)
			continue
		}
		out = append(out, tgField{name: m[1], kind: m[2], class: m[4]})
		if m[5] == "]" {
			if !inGate {
				t.Errorf("%s: %q closes a gate that is not open", row, tok)
			}
			inGate = false
		} else if inGate {
			gateLen++
		}
	}
	if inGate {
		t.Errorf("%s: a gate is never closed (%d fields in it)", row, gateLen+1)
	}
	return out
}

// tgTSV splits a tab-separated file into rows after checking its header and the column count.
func tgTSV(t *testing.T, name, src, header string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(src, "\n"), "\n")
	if len(lines) == 0 || lines[0] != header {
		t.Fatalf("%s: the header is not %q", name, header)
	}
	want := len(strings.Split(header, "\t"))
	var rows [][]string
	for i, l := range lines[1:] {
		cols := strings.Split(l, "\t")
		if len(cols) != want {
			t.Errorf("%s line %d: %d tab-separated fields, want %d", name, i+2, len(cols), want)
			continue
		}
		rows = append(rows, cols)
	}
	return rows
}

// TestTelegramFactsCiteTheirSource pins the provenance and the shape of the Telegram constructor and fact files
// (plan 4I4 Task 1; spec §13 second bullet; 4I4-P3 b, 4I4-P4; T2, T3, T5, T6; F25). Every constructors.tsv row
// cites <File>.java:<line> at one of four pins and holds fields of the 4I4-P4 grammar; the per-type counts are
// the pinned ones; key material (key, iv, file_reference, encrypted_key) is kept as length only (spec §9) while every access_hash stays i64 (the parser
// keeps its presence, spec §8.5); every inline id (T5) has its wrapper.* fact and every wrapper.* fact its token;
// CROSSCHECK.md reports every row verified (or read by hand), every ID of the three older pins in the table, and
// nothing missing or mismatched; no file holds a URL (GPLv2 facts only, cited by repository and commit).
func TestTelegramFactsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "telegram-android")
	consSrc := calendarReadFile(t, filepath.Join(dir, "constructors.tsv"))
	factsSrc := calendarReadFile(t, filepath.Join(dir, "facts.tsv"))
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))

	cons := tgTSV(t, "constructors.tsv", consSrc, telegramConstructorsHeader)
	facts := tgTSV(t, "facts.tsv", factsSrc, telegramFactsHeader)

	seen := map[string]bool{}
	counts := map[string]int{}
	// inline[class] lists "<field>=<class>" for every inline id token of that constructor class.
	inline := map[string][]string{}
	for _, r := range cons {
		typ, id, class, body, pin, source, fields := r[0], r[1], r[2], r[3], r[4], r[5], r[6]
		row := typ + " " + id + " " + class
		if !tgIDRE.MatchString(id) {
			t.Errorf("%s: the id is not 0x and 8 lowercase hex digits", row)
		}
		if seen[id] {
			t.Errorf("%s: the id is not unique", row)
		}
		seen[id] = true
		counts[typ]++
		if _, ok := telegramTypeCounts[typ]; !ok {
			t.Errorf("%s: type %q is not one of the pinned types (telegramTypeCounts)", row, typ)
		}
		if !slices.Contains([]string{"full", "prefix", "opaque"}, body) {
			t.Errorf("%s: body %q is not full, prefix or opaque", row, body)
		}
		if _, ok := telegramPins[pin]; !ok {
			t.Errorf("%s: pin %q is not one of the four", row, pin)
		}
		if !tgSourceRE.MatchString(source) {
			t.Errorf("%s: source %q is not <File>.java:<line>", row, source)
		}
		if body == "opaque" {
			if fields != "" {
				t.Errorf("%s: an opaque row lists fields", row)
			}
			continue
		}
		if fields == "" && body == "prefix" {
			t.Errorf("%s: a prefix row lists no fields", row)
		}
		for _, f := range tgParseFields(t, row, fields) {
			leaf := f.name[strings.LastIndex(f.name, ".")+1:]
			switch {
			case leaf == "access_hash" && f.kind != "i64":
				t.Errorf("%s: access_hash is %q, want i64 (presence is kept, spec §8.5; it is no key)", row, f.kind)
			case (leaf == "key" || leaf == "iv" || leaf == "file_reference" || leaf == "encrypted_key") && f.kind != "secret":
				t.Errorf("%s: %s is %q, want secret (length only, spec §9)", row, f.name, f.kind)
			}
			if f.class != "" {
				inline[class] = append(inline[class], f.name+"="+f.class)
			}
		}
	}
	for typ, n := range telegramTypeCounts {
		if counts[typ] != n {
			t.Errorf("type %s: %d rows, want %d", typ, counts[typ], n)
		}
	}
	byID := map[string][]string{}
	for _, r := range cons {
		byID[r[1]] = r
	}
	for id, class := range telegramRequiredIDs {
		if r, ok := byID[id]; !ok {
			t.Errorf("required id %s (%s) is not in constructors.tsv", id, class)
		} else if r[2] != class {
			t.Errorf("required id %s: class %q, want %q", id, r[2], class)
		}
	}
	// coordinator ruling M3 (Task 5 review): a constructor that reads no field is read whole, so it is full with an
	// empty field list, never opaque
	for _, id := range []string{"0x3ded6320", "0x9f84f49e", "0x85849473"} {
		if r, ok := byID[id]; !ok || r[3] != "full" || r[6] != "" {
			t.Errorf("constructor %s: %v, want a full row with an empty field list (ruling M3)", id, r)
		}
	}

	// facts.tsv: the fixed keys at their pins, the value domains, and the wrapper rows (T5, F25).
	have := map[string]bool{}
	wrappers := map[string][]string{}
	for _, r := range facts {
		key, value, pin, source := r[0], r[1], r[2], r[3]
		have[key+"@"+pin] = true
		if _, ok := telegramPins[pin]; !ok {
			t.Errorf("fact %s: pin %q is not one of the four", key, pin)
		}
		if !tgFactSourceRE.MatchString(source) {
			t.Errorf("fact %s: source %q is not <File>.java:<line>[-<line>]", key, source)
		}
		if value == "" {
			t.Errorf("fact %s: the value is empty", key)
		}
		switch {
		case strings.HasPrefix(key, "unit."):
			if value != "seconds" && value != "unestablished" {
				t.Errorf("fact %s: %q is not seconds or unestablished (T6, Q3)", key, value)
			}
		case key == "cast.encrypted_chat_id":
			if value != "int32" && value != "uint32" {
				t.Errorf("fact %s: %q is not int32 or uint32", key, value)
			}
		case strings.HasPrefix(key, "wrapper."):
			if !slices.Contains(tgWrapperValues, value) {
				t.Errorf("fact %s: %q is not user, chat, channel or unknown (T5)", key, value)
			}
			rest := strings.TrimPrefix(key, "wrapper.")
			dot := strings.Index(rest, ".")
			if dot < 0 {
				t.Errorf("fact %s: not wrapper.<Class>.<field>", key)
				continue
			}
			wrappers[rest[:dot]] = append(wrappers[rest[:dot]], rest[dot+1:]+"="+value)
		case strings.HasPrefix(key, "write_verb."):
			// The verb as the app writes it (T2): UPDATE, or REPLACE/INSERT [OR <conflict>] followed by its
			// target keyword. Checked word by word: the whole phrase as a literal would read as an SQL write to
			// TestOnlyRecordsPackageWritesRecordTables.
			words := strings.Fields(value)
			verb := value == "UPDATE" || len(words) >= 2 && slices.Contains([]string{"REPLACE", "INSERT"}, words[0]) &&
				words[len(words)-1] == "INTO" && value == strings.ToUpper(value)
			if !verb {
				t.Errorf("fact %s: %q is not an SQL verb as written (T2)", key, value)
			}
		}
	}
	for key, pins := range telegramFactKeys {
		for _, pin := range pins {
			if !have[key+"@"+pin] {
				t.Errorf("facts.tsv has no row %s at %s", key, pin)
			}
		}
	}
	// F25: the inline ids of constructors.tsv and the wrapper.* rows of facts.tsv agree both ways, as multisets
	// per class (an old forward header reads two inline ids into the same field, one per flag bit).
	classes := map[string]bool{}
	for c := range inline {
		classes[c] = true
	}
	for c := range wrappers {
		classes[c] = true
	}
	for c := range classes {
		a, b := slices.Clone(inline[c]), slices.Clone(wrappers[c])
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Errorf("class %s: inline ids %v in constructors.tsv, wrapper facts %v in facts.tsv", c, a, b)
		}
	}

	// CROSSCHECK.md: every row verified or read by hand, the older pins' membership, the facts read.
	for _, bad := range []string{"MISMATCH", "missing"} {
		if strings.Contains(report, bad) {
			t.Errorf("CROSSCHECK.md holds %q", bad)
		}
	}
	if !strings.HasPrefix(report, "# TL facts cross-check (plan 4I4)\n") {
		t.Errorf("CROSSCHECK.md does not start with the heading \"# TL facts cross-check (plan 4I4)\"")
	}
	for pin, commit := range telegramPins {
		if !strings.Contains(report, "| "+pin+" | "+commit+" |") {
			t.Errorf("CROSSCHECK.md has no pin row for %s at %s", pin, commit)
		}
	}
	for _, r := range cons {
		re := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(r[0]) + ` \| ` + regexp.QuoteMeta(r[1]) + ` \| ` +
			regexp.QuoteMeta(r[2]) + ` \| (verified|MANUAL, read by hand at [A-Za-z_]+\.java:[0-9]+) \| ` +
			regexp.QuoteMeta(r[5]) + ` \|`)
		if !re.MatchString(report) {
			t.Errorf("CROSSCHECK.md has no verified or read-by-hand row for %s %s %s at %s", r[0], r[1], r[2], r[5])
		}
	}
	memberRE := regexp.MustCompile(`(?m)^\| (A-11\.4\.2|release-8\.1\.1_2431|release-5\.14\.0_1851) \| (\w+) \| (0x[0-9a-f]{8}) \| in-table \|`)
	perPin := map[string]int{}
	for _, m := range memberRE.FindAllStringSubmatch(report, -1) {
		perPin[m[1]]++
		if r, ok := byID[m[3]]; !ok || r[0] != m[2] {
			t.Errorf("CROSSCHECK.md says %s %s %s is in the table; constructors.tsv disagrees", m[1], m[2], m[3])
		}
	}
	for _, pin := range []string{"A-11.4.2", "release-8.1.1_2431", "release-5.14.0_1851"} {
		if perPin[pin] == 0 {
			t.Errorf("CROSSCHECK.md has no in-table row at %s", pin)
		}
	}
	for _, r := range facts {
		row := "| " + r[0] + " | " + r[2] + " | " + r[3] + " | " + r[1] + " |"
		if !strings.Contains(report, row) {
			t.Errorf("CROSSCHECK.md has no row %q for the fact", row)
		}
	}

	// Review I1: each pin's report names the SHA-256 of the two fact files it checked; both must be the committed
	// bytes, so an edit of a field, a flag bit or a fact value without a new --check run fails here.
	hashRE := regexp.MustCompile(`(?m)^\| ` + "`" + `(constructors|facts)\.tsv` + "`" + ` \| ([0-9a-f]{64}) \|$`)
	want := map[string]string{"constructors": tgSHA256(consSrc), "facts": tgSHA256(factsSrc)}
	seenHash := map[string]int{}
	for _, m := range hashRE.FindAllStringSubmatch(report, -1) {
		seenHash[m[1]]++
		if m[2] != want[m[1]] {
			t.Errorf("CROSSCHECK.md checked %s.tsv with SHA-256 %s; the committed file is %s: run --check again",
				m[1], m[2], want[m[1]])
		}
	}
	for name := range want {
		if seenHash[name] != len(telegramPins) {
			t.Errorf("CROSSCHECK.md names the SHA-256 of %s.tsv %d times, want once per pin (%d)", name,
				seenHash[name], len(telegramPins))
		}
	}

	for name, src := range map[string]string{"constructors.tsv": consSrc, "facts.tsv": factsSrc, "CROSSCHECK.md": report} {
		if lines := calendarURLLines(src); len(lines) > 0 {
			t.Errorf("%s holds a URL (cite by repository and commit): %q", name, lines)
		}
	}
}
