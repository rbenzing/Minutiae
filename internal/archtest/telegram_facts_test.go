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
// Plan 4I4b Task 1 (design D1, D3, rulings R2, B2, B7, B9): every ID the A-HEAD switch of a type a full or prefix
// row names is a row (the closure), MessageAction included (116); the media sub-objects, the tail objects and the
// closure types; 535 rows in all (ruling T1d: the emitted counts).
// Plan 4I4c Task 2 (design D1-D4, §7; rulings Q5, T1d, T2a): reply markup, polls, InputUser and their closure; the
// keyboard types spelled TL_keyboard.<Name> (D2, B7); 0x11c1a322 TL_keyboardInlineButton, read ahead of the legacy
// switch, makes KeyboardInlineButton 26 (T2a); 629 rows in all.
var telegramTypeCounts = map[string]int{
	"Message": 49, "Peer": 6, "MessageFwdHeader": 7, "MessageAction": 116, "PhoneCallDiscardReason": 6,
	"MessageMedia": 52, "User": 29, "Chat": 39, "GeoPoint": 4, "MessageReplyHeader": 9, "MessageEntity": 28,
	"Photo": 8, "Document": 10, "WebPage": 12,
	"PhotoSize": 9, "VideoSize": 5, "DocumentAttribute": 17, "FileLocation": 5, "InputStickerSet": 12,
	"TL_maskCoords": 1, "Video": 6, "Audio": 5, "MessageReplies": 2, "MessageReactions": 4, "ReactionCount": 2,
	"Reaction": 4, "MessagePeerReaction": 4, "MessageReactor": 2, "RestrictionReason": 1, "TL_factCheck": 1,
	"TL_textWithEntities": 1, "SuggestedPost": 1, "TL_stars.StarsAmount": 2, "TL_messages_emojiGameOutcome": 1,
	"WebPageAttribute": 9, "Poll": 6, "PollResults": 6, "ReplyMarkup": 8, "InputUser": 6, "TL_iv.RichText": 32,
	"TL_iv.Page": 7, "TL_iv.RichMessage": 1,
	"TL_keyboard.KeyboardButtonRow": 1, "TL_keyboard.KeyboardInlineButtonRow": 2, "TL_keyboard.KeyboardButton": 16,
	"TL_keyboard.KeyboardInlineButton": 26, "TL_keyboard.KeyboardButtonStyle": 1, "TL_keyboard.ButtonType": 7,
	"TL_keyboard.InlineButtonType": 12, "RequestPeerType": 4, "TL_chatAdminRights": 1, "InputPeer": 12,
	"InlineQueryPeerType": 6, "PollAnswer": 4, "PollAnswerVoters": 2,
}

// telegramOpaqueCounts are the opaque rows per type (plan 4I4b Task 1; D1, R3, B2, B9); a type not listed has none.
// The closure types are opaque except a class that reads nothing (rule M3); an ID the pins read two ways is opaque
// (R3, T1b: 0x6fb250d1, 0x85849473, 0x332ba9ed). Plan 4I4c Task 2 (D1, D7, §10, P7, Q5): Poll, PollResults,
// InputUser and ReplyMarkup are read whole (0x48a30254 too: its older reading is a compatible widening, Q5); the ten
// client-to-server classes stay opaque (all-full-except).
var telegramOpaqueCounts = map[string]int{
	"MessageMedia": 19, "MessageAction": 103, "WebPage": 1, "WebPageAttribute": 6, "ReactionCount": 1,
	"TL_iv.RichText": 31, "TL_iv.Page": 7, "TL_iv.RichMessage": 1, "TL_keyboard.KeyboardButton": 2,
	"TL_keyboard.KeyboardInlineButton": 4, "TL_keyboard.ButtonType": 1, "TL_keyboard.InlineButtonType": 2, "PollAnswer": 1,
}

// telegramPrefixCounts are the prefix rows per type (spec Q2, T6); a type not listed has none.
var telegramPrefixCounts = map[string]int{"User": 25, "Chat": 32}

// tgKey is a row's identity: (type, ID) (plan 4I4c W1, design D4).
type tgKey struct{ typ, id string }

// telegramRequiredIDs are (type, ID) pairs every later task relies on, with the class the recon names (recon
// §3.2-§3.6); plan 4I4c Task 2 (design §8.1, D4, ruling T2a) adds the Stage B pairs, 0x77608b83 under both its types.
var telegramRequiredIDs = map[tgKey]string{
	{"MessageAction", "0x80e11a7f"}:          "TL_messageActionPhoneCall",
	{"PhoneCallDiscardReason", "0x85e42301"}: "TL_phoneCallDiscardReasonMissed",
	{"PhoneCallDiscardReason", "0xfaf7e8c9"}: "TL_phoneCallDiscardReasonBusy",
	{"PhoneCallDiscardReason", "0x57adc690"}: "TL_phoneCallDiscardReasonHangup",
	{"PhoneCallDiscardReason", "0xe095c1a0"}: "TL_phoneCallDiscardReasonDisconnect",
	{"PhoneCallDiscardReason", "0x9fbbf1f7"}: "TL_phoneCallDiscardReasonMigrateConferenceCall",
	{"Message", "0x555555fa"}:                "TL_message_secret",
	{"Message", "0x555555f9"}:                "TL_message_secret_layer72",
	{"Message", "0x555555f8"}:                "TL_message_secret_old",
	{"Message", "0x7600b9d3"}:                "TL_message",
	{"Message", "0x7a800e0a"}:                "TL_messageService",
	{"Message", "0x90a6ca84"}:                "TL_messageEmpty",
	{"Message", "0xa4e97f37"}:                "TL_message_layer179",
	{"Peer", "0x59511722"}:                   "TL_peerUser",
	{"Peer", "0x36c6019a"}:                   "TL_peerChat",
	{"Peer", "0xa2a5371e"}:                   "TL_peerChannel",
	{"Peer", "0x9db1bc6d"}:                   "TL_peerUser_layer131",
	{"Peer", "0xbad0e5bb"}:                   "TL_peerChat_layer131",
	{"Peer", "0xbddde532"}:                   "TL_peerChannel_layer131",
	{"Document", "0x55555558"}:               "TL_documentEncrypted",
	// plan 4I4b Task 1 (design §7, recon §2): the file name attribute, the 5.14.0-era media path, the tail objects
	{"DocumentAttribute", "0x15590068"}: "TL_documentAttributeFilename",
	{"MessageMedia", "0x695150d7"}:      "TL_messageMediaPhoto_layer223",
	{"MessageMedia", "0x9cb070d7"}:      "TL_messageMediaDocument_layer159",
	{"Photo", "0xd07504a5"}:             "TL_photo_layer115",
	{"Document", "0x9ba29cc1"}:          "TL_document_layer113",
	{"PhotoSize", "0x77bfb61b"}:         "TL_photoSize_layer127",
	{"FileLocation", "0x091d11eb"}:      "TL_fileLocation_layer97",
	{"FileLocation", "0x55555554"}:      "TL_fileEncryptedLocation",
	{"Document", "0x55555556"}:          "TL_documentEncrypted_old",
	{"ReactionCount", "0x6fb250d1"}:     "TL_reactionCount_layer144",
	{"ReactionCount", "0xa3d1cb80"}:     "TL_reactionCount",
	{"MessageReactions", "0x0a339f0b"}:  "TL_messageReactions",
	{"MessageReplies", "0x83d60fc2"}:    "TL_messageReplies",
	{"SuggestedPost", "0x0e8e37e5"}:     "SuggestedPost",
	// plan 4I4c Task 2 (design §8.1, D4; ruling T2a)
	{"TL_keyboard.KeyboardButtonRow", "0x77608b83"}:       "KeyboardButtonRow",
	{"TL_keyboard.KeyboardInlineButtonRow", "0x77608b83"}: "TL_keyboardInlineButtonRow_layer228",
	{"TL_keyboard.KeyboardInlineButtonRow", "0x19420af6"}: "TL_keyboardInlineButtonRow",
	{"TL_keyboard.KeyboardInlineButton", "0x11c1a322"}:    "TL_keyboardInlineButton",
	{"InputUser", "0xf21158c6"}:                           "TL_inputUser",
	{"Poll", "0x966e2dbf"}:                                "TL_poll",
	{"PollResults", "0xba7bb15e"}:                         "TL_pollResults",
	{"ReplyMarkup", "0xb2b15770"}:                         "TL_replyInlineMarkup",
	{"ReplyMarkup", "0x85dd99d1"}:                         "TL_replyKeyboardMarkup",
	{"TL_keyboard.InlineButtonType", "0x2955bc38"}:        "TL_inlineButtonTypeCallback",
	{"TL_keyboard.KeyboardInlineButton", "0xe62bc960"}:    "TL_keyboardButtonCallback_layer228",
}

// telegramFactKeys are the fixed facts.tsv keys (Task 1), each with the pins it must be stated at.
var telegramFactKeys = map[string][]string{
	"write_verb.messages_v2.put":  {"A-HEAD", "A-11.4.2", "release-8.1.1_2431"},
	"write_verb.messages_v2.edit": {"A-HEAD", "A-11.4.2", "release-8.1.1_2431"},
	"write_verb.messages.put":     {"release-5.14.0_1851"},
	"write_verb.messages.edit":    {"release-5.14.0_1851"},
	"unit.call_duration":          {"A-HEAD"},
	"unit.edit_date":              {"A-HEAD", "A-11.4.2", "release-8.1.1_2431", "release-5.14.0_1851"},
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
	// fields and closes on the "]" that ends its last field; gates do not nest. Plan 4I4c (design W2, W3, P6, P8):
	// a classed id vector vec<id32@C>/vec<id64@C> (its class captured like an inline id's, so F25 asks for its
	// wrapper fact), a string vector vec<str> and a two-type union vector vec<A|B>.
	tgGateOpenRE = regexp.MustCompile(`^\[F2?\.[0-9]+:$`)
	tgName       = `[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`
	tgType       = `[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`
	tgFieldRE    = regexp.MustCompile(`^(` + tgName + `)(?:=F2?\.[0-9]+|:(#|i32|i64|dbl|bool|bytes|str|secret|` +
		`(?:id32|id64)@(user|chat|channel|unknown)|vec<(?:id32|id64)@(user|chat|channel|unknown)>|vec<i32>|vec<i64>|` +
		`vec<str>|vec<` + tgType + `\|` + tgType + `>|vec<` + tgType + `>|` + tgType + `))(\]?)$`)
	// Kinds no row may hold (ruling C6: the generator cannot emit them; a union is two types inside vec<...> only).
	tgForbiddenKindRE = regexp.MustCompile(`^vec<(bytes|dbl|bool|secret|#)>$`)
	tgWrapperValues   = []string{"user", "chat", "channel", "unknown"}
	// A field type StarsAmount not qualified by its outer class (ruling B7).
	tgBareStarsRE = regexp.MustCompile(`(^|[^.\w])StarsAmount\b`)
)

// tgScalarKinds are the scalar kinds of the grammar: never an object type, never a member of a union.
var tgScalarKinds = map[string]bool{"#": true, "i32": true, "i64": true, "dbl": true, "bool": true, "bytes": true, "str": true, "secret": true}

// tgKindDefect is why a field kind the token grammar accepts is still outside it (ruling C6), "" when it is not: a
// vector of a scalar the walker has no reader for, or a union whose two members are equal or one is a scalar.
func tgKindDefect(kind string) string {
	if tgForbiddenKindRE.MatchString(kind) {
		return "a vector of " + kind[4:len(kind)-1]
	}
	inner, ok := strings.CutPrefix(kind, "vec<")
	if !ok {
		return ""
	}
	a, b, union := strings.Cut(strings.TrimSuffix(inner, ">"), "|")
	switch {
	case !union:
		return ""
	case a == b:
		return "a union of one type twice"
	case tgScalarKinds[a] || tgScalarKinds[b]:
		return "a scalar inside a union"
	}
	return ""
}

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
		class := m[3]
		if m[4] != "" {
			class = m[4]
		}
		if why := tgKindDefect(m[2]); why != "" {
			t.Errorf("%s: %q holds a kind outside the grammar (ruling C6): %s", row, tok, why)
		}
		out = append(out, tgField{name: m[1], kind: m[2], class: class})
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
// Plan 4I4b Task 1 (design D1, D3-D6, D9; rulings R3, B2-B4, B7, B9, B11, T1a-T1d): the closed table's counts per
// type and body, the pin conflicts as opaque rows and the only CONFLICT verdicts, the D5 field type, the B7 spelling,
// unit.edit_date seconds at all four pins, the compatible widening of 0xe89c45b2 and the hand-read section; plan 4I4b
// Task 2 (ruling B11): the second, independent hand-read agrees with each of the four rows token for token.
// Plan 4I4c Task 2 (design D1-D4, W2, W3, §7, §8; Q5, Q10; rulings C6, T1d, T2a, T2b; P8): rows keyed by (type, ID)
// with 0x77608b83 the one shared ID, the union, classed id and string vectors of the grammar, the 629-row counts,
// 0x48a30254 full as a compatible widening (union), membership through the button unions and the hand-read section.
// Plan 4I4c Task 3 (rulings B11, T2a): the second, independent hand-read agrees with each of the six rows.
func TestTelegramFactsCiteTheirSource(t *testing.T) {
	dir := calendarSchemaDir(t, "telegram-android")
	consSrc := calendarReadFile(t, filepath.Join(dir, "constructors.tsv"))
	factsSrc := calendarReadFile(t, filepath.Join(dir, "facts.tsv"))
	report := calendarReadFile(t, filepath.Join(dir, "CROSSCHECK.md"))

	cons := tgTSV(t, "constructors.tsv", consSrc, telegramConstructorsHeader)
	facts := tgTSV(t, "facts.tsv", factsSrc, telegramFactsHeader)

	// plan 4I4c W1, design D4: a row is identified by (type, ID); one ID may be a row of two types.
	seen := map[tgKey]bool{}
	typesOfID := map[string][]string{}
	counts, opaque, prefix := map[string]int{}, map[string]int{}, map[string]int{}
	// inline[class] lists "<field>=<class>" for every inline id token of that constructor class.
	inline := map[string][]string{}
	for _, r := range cons {
		typ, id, class, body, pin, source, fields := r[0], r[1], r[2], r[3], r[4], r[5], r[6]
		row := typ + " " + id + " " + class
		if !tgIDRE.MatchString(id) {
			t.Errorf("%s: the id is not 0x and 8 lowercase hex digits", row)
		}
		if seen[tgKey{typ, id}] {
			t.Errorf("%s: the (type, id) pair is not unique (design D4)", row)
		}
		seen[tgKey{typ, id}] = true
		typesOfID[id] = append(typesOfID[id], typ)
		counts[typ]++
		switch body {
		case "opaque":
			opaque[typ]++
		case "prefix":
			prefix[typ]++
		}
		// ruling B7: the type spelling is the field's (TL_stars.StarsAmount) everywhere
		if typ == "StarsAmount" || tgBareStarsRE.MatchString(fields) {
			t.Errorf("%s: StarsAmount is spelled without TL_stars. (ruling B7)", row)
		}
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
	for typ := range telegramTypeCounts {
		if opaque[typ] != telegramOpaqueCounts[typ] {
			t.Errorf("type %s: %d opaque rows, want %d (plan 4I4b Task 1, D1)", typ, opaque[typ], telegramOpaqueCounts[typ])
		}
		if prefix[typ] != telegramPrefixCounts[typ] {
			t.Errorf("type %s: %d prefix rows, want %d (spec Q2, T6)", typ, prefix[typ], telegramPrefixCounts[typ])
		}
	}
	// design D4: exactly one ID is a row of two types, 0x77608b83 (TL_keyboard.java:749 and :645); no other is shared.
	for id, typs := range typesOfID {
		if id == "0x77608b83" {
			slices.Sort(typs)
			if want := []string{"TL_keyboard.KeyboardButtonRow", "TL_keyboard.KeyboardInlineButtonRow"}; !slices.Equal(typs, want) {
				t.Errorf("id 0x77608b83 is a row of %v, want %v (design D4)", typs, want)
			}
		} else if len(typs) != 1 {
			t.Errorf("id %s is a row of %v; only 0x77608b83 is shared (design D4)", id, typs)
		}
	}
	byKey := map[tgKey][]string{}
	for _, r := range cons {
		byKey[tgKey{r[0], r[1]}] = r
	}
	// byID is the row of an ID that is a row of one type only (every ID but 0x77608b83).
	byID := func(id string) ([]string, bool) {
		if typs := typesOfID[id]; len(typs) == 1 {
			return byKey[tgKey{typs[0], id}], true
		}
		return nil, false
	}
	for k, class := range telegramRequiredIDs {
		if r, ok := byKey[k]; !ok {
			t.Errorf("required (type, id) %s %s (%s) is not in constructors.tsv", k.typ, k.id, class)
		} else if r[2] != class {
			t.Errorf("required (type, id) %s %s: class %q, want %q", k.typ, k.id, r[2], class)
		}
	}
	// coordinator ruling M3 (Task 5 review): a constructor that reads no field is read whole, so it is full with an
	// empty field list, never opaque
	// (plan 4I4b Task 1, rulings B9 and B2: the closure classes and the MessageAction classes that read nothing).
	m3 := []string{
		"0x3ded6320", "0x9f84f49e", "0xb98886cf", "0xf7c1b13f", "0xdc3d824f",
		"0x01baa035", "0x4792929b", "0x55555550", "0x55555557", "0x94bd38ed", "0x95e3fbef",
		"0x9fbab604", "0xb6aef7b0", "0xc1d2f1c6", "0xe7e75f97", "0xebbca3cb", "0xf3f25f76",
		// plan 4I4c Task 2 (design §7): the InlineQueryPeerType, InputPeer, ButtonType and InlineButtonType classes
		// that read nothing
		"0x0e3b2d0c", "0x3081ed9d", "0x5ec4be43", "0x6334ee9a", "0x833c0fac", "0xd766c50a", "0x9beee140",
		"0xc9dd90e9", "0xdf3d36f9", "0x48bad7a5", "0x5cd3709d", "0xa438619d", "0x7da07ec9", "0x7f3b18ea",
	}
	for _, id := range m3 {
		if r, ok := byID(id); !ok || r[3] != "full" || r[6] != "" {
			t.Errorf("constructor %s: %v, want a full row with an empty field list (ruling M3)", id, r)
		}
	}

	// rulings R3 and T1b: an ID the pinned app versions read two ways is opaque; the table never picks one reading.
	// 0x85849473 was a full row in 4I4 (the older pins read flags and cached_page_views for it, A-HEAD nothing).
	for _, id := range []string{"0x6fb250d1", "0x85849473", "0x332ba9ed"} {
		if r, ok := byID(id); !ok || r[3] != "opaque" {
			t.Errorf("constructor %s: %v, want an opaque row (rulings R3, T1b; design D4)", id, r)
		}
	}
	// plan 4I4c Task 2 (Q5, design §8.3): every older inline keyboard is readable by the A-HEAD reading of
	// 0x48a30254 (each older button ID is a row of one of the two button types and reads the same bytes), so it is
	// full, a compatible widening; ruling T2a: every button reader of A-HEAD reads both button families, in the app's
	// order (TL_keyboard.java:561-575, :735-742, :815-822).
	for k, want := range map[tgKey]string{
		{"ReplyMarkup", "0x48a30254"}:                         "rows:vec<TL_keyboard.KeyboardInlineButtonRow>",
		{"TL_keyboard.KeyboardButtonRow", "0x77608b83"}:       "buttons:vec<TL_keyboard.KeyboardInlineButton|TL_keyboard.KeyboardButton>",
		{"TL_keyboard.KeyboardInlineButtonRow", "0x77608b83"}: "buttons:vec<TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton>",
		{"TL_keyboard.KeyboardInlineButtonRow", "0x19420af6"}: "buttons:vec<TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton>",
	} {
		if r, ok := byKey[k]; !ok || r[3] != "full" || r[6] != want {
			t.Errorf("constructor %s %s: %v, want a full row %q (Q5, ruling T2a)", k.typ, k.id, r, want)
		}
	}
	// design D5, ruling R4: a field typed by a class with no deserializer of its own names the switch's type.
	unbracket := strings.NewReplacer("[", " ", "]", " ")
	for _, id := range []string{"0x7a800e0a", "0xd3d28540"} {
		if r, ok := byID(id); !ok || !slices.Contains(strings.Fields(unbracket.Replace(r[6])), "reactions:MessageReactions") {
			t.Errorf("constructor %s: %v, want the field reactions:MessageReactions (design D5)", id, r)
		}
	}
	for _, r := range cons {
		if strings.Contains(r[6], ":TL_messageReactions") || strings.Contains(r[6], "<TL_messageReactions>") {
			t.Errorf("%s %s %s: a field is typed TL_messageReactions; the type is MessageReactions (design D5)",
				r[0], r[1], r[2])
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
			// ruling B3: the edit_date route is seconds at every pin
			if key == "unit.edit_date" && value != "seconds" {
				t.Errorf("fact %s at %s: %q, want seconds (ruling B3, design D9)", key, pin, value)
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
	// plan 4I4c (design D4, §8.3; ruling T2a): membership is per (type, ID).
	const olderPins = `(A-11\.4\.2|release-8\.1\.1_2431|release-5\.14\.0_1851)`
	memberRE := regexp.MustCompile(`(?m)^\| ` + olderPins + ` \| ([\w.]+) \| (0x[0-9a-f]{8}) \| in-table \|`)
	perPin := map[string]int{}
	for _, m := range memberRE.FindAllStringSubmatch(report, -1) {
		perPin[m[1]]++
		if _, ok := byKey[tgKey{m[2], m[3]}]; !ok {
			t.Errorf("CROSSCHECK.md says %s %s %s is in the table; constructors.tsv disagrees", m[1], m[2], m[3])
		}
	}
	// ruling T2a: an older ID of one type that A-HEAD reads as the other type of every union field naming its type
	// is in the table through that union; a narrowing (a field no union reaches) would name an ID with no row of its
	// own type and a row of another. Neither may claim a row the table lacks, and the facts produce no narrowing.
	unionRE := regexp.MustCompile(`(?m)^\| ` + olderPins + ` \| ([\w.]+) \| (0x[0-9a-f]{8}) \| ` +
		`(in-table through the union|narrowing) \(A-HEAD reads it as ([\w.]+)[;,)]`)
	unionAt := map[string]int{}
	for _, m := range unionRE.FindAllStringSubmatch(report, -1) {
		unionAt[m[1]+" "+m[4]]++
		if _, own := byKey[tgKey{m[2], m[3]}]; own {
			t.Errorf("CROSSCHECK.md %s %s %s %s: the ID is a row of its own type", m[1], m[2], m[3], m[4])
		}
		if _, other := byKey[tgKey{m[5], m[3]}]; !other || m[5] == m[2] {
			t.Errorf("CROSSCHECK.md %s %s %s %s: no row of type %s holds it", m[1], m[2], m[3], m[4], m[5])
		}
	}
	if unionAt["release-5.14.0_1851 in-table through the union"] == 0 {
		t.Errorf("CROSSCHECK.md has no union membership line at release-5.14.0_1851 (ruling T2a)")
	}
	if n := strings.Count(report, "| narrowing ("); n != 0 {
		t.Errorf("CROSSCHECK.md holds %d narrowing verdicts, want 0: every A-HEAD button reader reads both button "+
			"families (ruling T2a)", n)
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

	// Plan 4I4b Task 1 (design D6, rulings B4, T1a, T1b, 4I4b-P7): every body CONFLICT of the older-pin comparison is
	// a pin conflict on an opaque row, exactly the IDs each pin reads another way; field names never make one (T1a).
	// Plan 4I4c Task 2 (Q5): 0x48a30254 left the pin conflicts as a compatible widening (union); 5 CONFLICTs remain.
	wantConflicts := map[string][]string{
		"A-11.4.2":            {"0x332ba9ed", "0x85849473"},
		"release-8.1.1_2431":  {"0x6fb250d1", "0x85849473"},
		"release-5.14.0_1851": {"0x6fb250d1"},
	}
	gotConflicts := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^\| ([^|]+) \| ([^|]+) \| (0x[0-9a-f]{8}) \| CONFLICT[^|]*\|.*$`).
		FindAllStringSubmatch(report, -1) {
		gotConflicts[m[1]] = append(gotConflicts[m[1]], m[3])
		r, ok := byKey[tgKey{m[2], m[3]}]
		if !strings.Contains(m[0], "| CONFLICT (opaque; pin conflict: ") || !ok || r[3] != "opaque" {
			t.Errorf("CROSSCHECK.md holds a CONFLICT that is not a pin conflict on an opaque row (ruling T1b): %q", m[0])
		}
	}
	if n := strings.Count(report, "| CONFLICT"); n != 5 {
		t.Errorf("CROSSCHECK.md holds %d CONFLICT verdicts, want 5 (rulings T1b, Q5)", n)
	}
	// Q5 (design §8.3): at each older pin the older reading of 0x48a30254 is a compatible widening through the union.
	for _, pin := range []string{"A-11.4.2", "release-8.1.1_2431", "release-5.14.0_1851"} {
		re := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(pin) + ` \| ReplyMarkup \| 0x48a30254 \| compatible widening \(union\) `)
		if !re.MatchString(report) {
			t.Errorf("CROSSCHECK.md has no compatible widening (union) of 0x48a30254 at %s (Q5)", pin)
		}
	}
	for pin, ids := range wantConflicts {
		slices.Sort(gotConflicts[pin])
		if !slices.Equal(gotConflicts[pin], ids) {
			t.Errorf("CROSSCHECK.md pin conflicts at %s: %v, want %v (rulings R3, T1b)", pin, gotConflicts[pin], ids)
		}
	}
	// ruling T1c: TL_webPage keeps the A-HEAD reading of its attributes as a compatible widening at the two pins that
	// read them by the single class TL_webPageAttributeTheme.
	for _, pin := range []string{"release-8.1.1_2431", "release-5.14.0_1851"} {
		if !regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(pin) + ` \| WebPage \| 0xe89c45b2 \| compatible widening \(`).
			MatchString(report) {
			t.Errorf("CROSSCHECK.md has no compatible widening of 0xe89c45b2 at %s (ruling T1c)", pin)
		}
	}
	// ruling B11: the hand-read rows of Task 1, one row each in their section.
	const handHead = "## Hand-read rows, first read (plan 4I4b Task 1)\n"
	if i := strings.Index(report, handHead); i < 0 {
		t.Errorf("CROSSCHECK.md has no section %q", strings.TrimSpace(handHead))
	} else {
		section := report[i+len(handHead):]
		if j := strings.Index(section, "\n#"); j >= 0 {
			section = section[:j]
		}
		for _, id := range []string{"0xa3d1cb80", "0x51b67eff", "0x932844fa", "0xfa64e172"} {
			if n := len(regexp.MustCompile(`(?m)^\| `+id+` \|`).FindAllString(section, -1)); n != 1 {
				t.Errorf("CROSSCHECK.md hand-read section: %d rows for %s, want 1 (ruling B11)", n, id)
			}
		}
	}
	// ruling B11 (plan 4I4b Task 2): the second, independent hand-read agrees with each committed row token for
	// token: one row per ID whose body read equals that ID's fields in constructors.tsv, verdict "agrees".
	const hand2Head = "## Hand-read rows, second read (plan 4I4b Task 2, B11)\n"
	if i := strings.Index(report, hand2Head); i < 0 {
		t.Errorf("CROSSCHECK.md has no section %q", strings.TrimSpace(hand2Head))
	} else {
		section := report[i+len(hand2Head):]
		if j := strings.Index(section, "\n#"); j >= 0 {
			section = section[:j]
		}
		for _, id := range []string{"0xa3d1cb80", "0x51b67eff", "0x932844fa", "0xfa64e172"} {
			r, ok := byID(id)
			if !ok {
				t.Errorf("constructors.tsv has no row %s (ruling B11)", id)
				continue
			}
			line := `(?m)^\| ` + id + ` \| ` + regexp.QuoteMeta(r[2]) + ` \| ` + regexp.QuoteMeta(r[5]) + ` \| ` +
				regexp.QuoteMeta(r[6]) + ` \| agrees \|$`
			if n := len(regexp.MustCompile(line).FindAllString(section, -1)); n != 1 {
				t.Errorf("CROSSCHECK.md second hand-read section: %d agreeing rows for %s %s %s, want 1 (ruling B11)",
					n, id, r[2], r[5])
			}
		}
	}

	// Plan 4I4c Task 2 (design §7, rulings B11, T2a): the Stage B rows the reduction cannot read (the union rows and
	// the classed id vectors), one row each, read by hand at their pinned line with the committed body; Task 3 reads
	// them a second time.
	const hand3Head = "## Hand-read rows, first read (plan 4I4c Task 2)\n"
	if i := strings.Index(report, hand3Head); i < 0 {
		t.Errorf("CROSSCHECK.md has no section %q", strings.TrimSpace(hand3Head))
	} else {
		section := report[i+len(hand3Head):]
		if j := strings.Index(section, "\n#"); j >= 0 {
			section = section[:j]
		}
		for _, k := range []tgKey{
			{"TL_keyboard.KeyboardInlineButtonRow", "0x77608b83"},
			{"TL_keyboard.KeyboardButtonRow", "0x77608b83"},
			{"TL_keyboard.KeyboardInlineButtonRow", "0x19420af6"},
			{"PollResults", "0xc87024a2"},
			{"PollResults", "0xbadcc1a3"},
			{"PollResults", "0xdcb82ea3"},
		} {
			r, ok := byKey[k]
			if !ok {
				t.Errorf("constructors.tsv has no row %s %s (ruling B11)", k.typ, k.id)
				continue
			}
			// a union's "|" is escaped in the Markdown table cell
			body := strings.ReplaceAll(r[6], "|", `\|`)
			line := `(?m)^\| ` + k.id + ` \| ` + regexp.QuoteMeta(r[2]) + ` \| ` + regexp.QuoteMeta(r[5]) + ` \| ` +
				regexp.QuoteMeta(body) + ` \| read by hand \|$`
			if n := len(regexp.MustCompile(line).FindAllString(section, -1)); n != 1 {
				t.Errorf("CROSSCHECK.md 4I4c hand-read section: %d rows for %s %s, want 1 (ruling B11)", n, k.id, r[2])
			}
		}
	}
	// Plan 4I4c Task 3 (rulings B11, T2a): the second, independent hand-read of the same six rows agrees with each
	// committed row token for token: one row per (type, ID) whose body read equals its fields, verdict "agrees".
	const hand4Head = "## Hand-read rows, second read (plan 4I4c Task 3, B11)\n"
	if i := strings.Index(report, hand4Head); i < 0 {
		t.Errorf("CROSSCHECK.md has no section %q", strings.TrimSpace(hand4Head))
	} else {
		section := report[i+len(hand4Head):]
		if j := strings.Index(section, "\n#"); j >= 0 {
			section = section[:j]
		}
		for _, k := range []tgKey{
			{"TL_keyboard.KeyboardInlineButtonRow", "0x77608b83"},
			{"TL_keyboard.KeyboardButtonRow", "0x77608b83"},
			{"TL_keyboard.KeyboardInlineButtonRow", "0x19420af6"},
			{"PollResults", "0xc87024a2"},
			{"PollResults", "0xbadcc1a3"},
			{"PollResults", "0xdcb82ea3"},
		} {
			r, ok := byKey[k]
			if !ok {
				t.Errorf("constructors.tsv has no row %s %s (ruling B11)", k.typ, k.id)
				continue
			}
			body := strings.ReplaceAll(r[6], "|", `\|`)
			line := `(?m)^\| ` + k.id + ` \| ` + regexp.QuoteMeta(r[2]) + ` \| ` + regexp.QuoteMeta(r[5]) + ` \| ` +
				regexp.QuoteMeta(body) + ` \| agrees \|$`
			if n := len(regexp.MustCompile(line).FindAllString(section, -1)); n != 1 {
				t.Errorf("CROSSCHECK.md 4I4c second hand-read section: %d agreeing rows for %s %s, want 1 (ruling B11)",
					n, k.id, r[2])
			}
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

// Plan 4I4c W2 and ruling C6 (Task 2 review, carried into Task 4): the token grammar's union vec<A|B> takes two
// distinct object types; a union of one type twice or with a scalar member, and a vector of a scalar without a reader,
// are outside it. The kinds the walker reads pass.
func TestTelegramFieldGrammarRefusesBadUnions(t *testing.T) {
	for _, kind := range []string{"vec<X|X>", "vec<str|Foo>", "vec<Foo|str>", "vec<bytes|Foo>", "vec<Foo|i64>", "vec<bytes>", "vec<dbl>"} {
		if tgKindDefect(kind) == "" {
			t.Errorf("%s: accepted, want a defect (ruling C6)", kind)
		}
	}
	for _, kind := range []string{
		"vec<TL_keyboard.KeyboardButton|TL_keyboard.KeyboardInlineButton>", "vec<str>", "vec<i32>",
		"vec<id32@user>", "vec<id64@user>", "vec<MessageEntity>", "i32", "Peer",
	} {
		if why := tgKindDefect(kind); why != "" {
			t.Errorf("%s: refused (%s), want accepted", kind, why)
		}
	}
}
