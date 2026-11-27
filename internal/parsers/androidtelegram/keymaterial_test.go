package androidtelegram_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// chainBytes is n bytes of the fixtures' key-material pattern (tools/fixtures/telegram_android.py chain): SHA-256 of
// the seed, then of each previous digest, concatenated. Restated here from the generator's rule, never read from it.
func chainBytes(seed string, n int) []byte {
	var out []byte
	h := []byte(seed)
	for len(out) < n {
		s := sha256.Sum256(h)
		h = s[:]
		out = append(out, h...)
	}
	return out[:n]
}

// syntheticPrefix is the marker every synthetic secret field of the fixtures starts with (telegram_tl_encode.py).
const syntheticPrefix = "SYNTHETIC-NOT-A-KEY-"

// keyPattern is one key-material byte pattern and the first of its 8-byte windows that holds a key byte (a secret
// field starts with the synthetic marker, which alone is no key).
type keyPattern struct {
	b     []byte
	first int
}

// generatorSource is the fixture generator whose secret-kind values the patterns are derived from (M7).
const generatorSource = "../../../tools/fixtures/telegram_android.py"

// keyPatterns are the fixtures' key material (spec 9, T1), derived from every literal secret(seed, n) and
// chain(seed, n) the generator fills (the enc_chats g, authkey, fauthkey, khash and data chains; a secret
// document's key and IV, file references and every other secret-kind field such as encrypted_key, the
// "4i4-layers" chain): a chain is its bytes; a secret field is the synthetic marker, then the chain, cut to
// max(n, len(marker)+4) bytes (telegram_android.py secret), searched whole from its first key-bearing window and, when
// it holds at least 8 chain bytes, as its chain part. The rule of each form is restated here, never imported.
func keyPatterns(t *testing.T) map[string]keyPattern {
	t.Helper()
	src, err := os.ReadFile(generatorSource)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]keyPattern{}
	re := regexp.MustCompile(`\b(secret|chain)\("([^"]+)", (\d+)\)`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		n, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatal(err)
		}
		if m[1] == "chain" {
			out["chain:"+m[2]] = keyPattern{b: chainBytes(m[2], n)}
			continue
		}
		field := append([]byte(syntheticPrefix), chainBytes(m[2], n)...)[:max(n, len(syntheticPrefix)+4)]
		out["secret:"+m[2]] = keyPattern{b: field, first: len(syntheticPrefix) - 7}
		if part := field[len(syntheticPrefix):]; len(part) >= 8 {
			out["secret-chain:"+m[2]] = keyPattern{b: part}
		}
	}
	for _, want := range []string{
		"chain:4i4-g", "chain:4i4-authkey", "chain:4i4-fauthkey", "chain:4i4-khash", "chain:4i4-encdata",
		"secret:4i4-dockey", "secret:4i4-dociv", "secret:4i4-layers",
	} {
		if _, ok := out[want]; !ok {
			t.Fatalf("the generator fills no %s: the patterns are not derived from it", want)
		}
	}
	return out
}

// secretI64Base is the base of secret_i64(n) in tools/fixtures/telegram_android.py (plan 4I4b, Fixture scenario),
// restated from the plan, never read from the generator.
const secretI64Base = 0x7E575EC2E7A10000

// keyInt64s are the int64 credentials of the fixtures (R7, D12): secret_i64(n) = secretI64Base + n for every n the
// generator names (a secret_i64(<literal>) and the id of every document(<id>, ...), whose access_hash is secret_i64(id);
// required: some variant must hold it) and every n a layers row can take (the rowid-numbered access_hash and secret of
// a nested object; searched, not required). The label is "i64:<n>".
func keyInt64s(t *testing.T) (required, searched map[string]uint64) {
	t.Helper()
	src, err := os.ReadFile(generatorSource)
	if err != nil {
		t.Fatal(err)
	}
	required, searched = map[string]uint64{}, map[string]uint64{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`secret_i64\((\d+)\)`), regexp.MustCompile(`[^_a-z]document\((\d+),`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			n, err := strconv.ParseUint(m[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			required["i64:"+m[1]] = secretI64Base + n
		}
	}
	// 1..4000 covers every rowid-numbered credential: the generator asserts 1701 + len(todo) <= 2001 (layers_rows, rowids
	// at most 2000) and the Stage B layers rows from 2101 are asserted < 2401 (plan 4I4c Task 5, rows 2101-2139), so no
	// layers rowid reaches 2401, and its secret_i64 arguments at layers rows stay below it (final-review item 4)
	for n := uint64(1); n <= 4000; n++ {
		if _, ok := required["i64:"+strconv.FormatUint(n, 10)]; !ok {
			searched["i64:"+strconv.FormatUint(n, 10)] = secretI64Base + n
		}
	}
	// plan 4I4c: every new access_hash is secret_i64(n) for n in 4401 up (STAGE_B_SECRETS = range(4401, 4500) in the
	// generator); searched, so a Stage B credential no literal names is still covered (R7)
	for n := uint64(4401); n < 4500; n++ {
		if _, ok := required["i64:"+strconv.FormatUint(n, 10)]; !ok {
			searched["i64:"+strconv.FormatUint(n, 10)] = secretI64Base + n
		}
	}
	if len(required) < 10 {
		t.Fatalf("the generator names only %d secret_i64 numbers: the patterns are not derived from it", len(required))
	}
	return required, searched
}

// int64Forms are the forms an int64 credential could take in an output: its decimal, its 16-digit hex in both cases
// with and without 0x, and the encoded forms of its 8 little-endian bytes.
func int64Forms(v uint64) []string {
	h := strconv.FormatUint(v, 16)
	le := binary.LittleEndian.AppendUint64(nil, v)
	out := []string{strconv.FormatUint(v, 10), h, strings.ToUpper(h), "0x" + h, "0x" + strings.ToUpper(h)}
	return append(out, encodedForms(le)...)
}

// fprint is the fixtures' enc_chats.fprint (a key fingerprint).
const fprint = 0x0123456789ABCDEF

// encodedForms are the forms a byte pattern could take in an output: raw, lower and upper hex, and standard and URL
// base64. A base64 form is given at each of the three byte alignments, cut to whole 3-byte groups, so it is found
// inside the encoding of any longer value that holds the pattern.
func encodedForms(p []byte) []string {
	h := hex.EncodeToString(p)
	out := []string{string(p), h, strings.ToUpper(h)}
	for a := 0; a < 3 && a < len(p); a++ {
		q := p[a:]
		q = q[:len(q)/3*3]
		if len(q) == 0 {
			continue
		}
		out = append(out, base64.RawStdEncoding.EncodeToString(q), base64.RawURLEncoding.EncodeToString(q))
	}
	return out
}

// needles are every form of every pattern and of each of its 8-byte windows from its first key-bearing one, by a
// label naming the pattern; fprint in decimal and hex.
func needles(ps map[string]keyPattern, ints ...map[string]uint64) map[string][]string {
	out := map[string][]string{}
	for _, m := range ints {
		for name, v := range m {
			out[name] = int64Forms(v)
		}
	}
	for name, p := range ps {
		out[name] = append(out[name], encodedForms(p.b)...)
		for i := p.first; i+8 <= len(p.b); i++ {
			out[name] = append(out[name], encodedForms(p.b[i:i+8])...)
		}
	}
	dec, h := strconv.FormatUint(fprint, 10), strconv.FormatUint(fprint, 16)
	out["enc_chats.fprint"] = []string{dec, h, strings.ToUpper(h)}
	return out
}

// finderKey is the length of the prefix the finder indexes: the shortest needle (an 8-byte window raw, or 6 of its
// bytes in base64).
const finderKey = 8

// finder finds any of many needles in a text in one pass: the needles by their first finderKey bytes.
type finder map[string][][2]string // prefix -> (label, needle)

func newFinder(ns map[string][]string) finder {
	f := finder{}
	for label, forms := range ns {
		for _, s := range forms {
			if len(s) < finderKey {
				panic("needle shorter than the finder key: " + s)
			}
			f[s[:finderKey]] = append(f[s[:finderKey]], [2]string{label, s})
		}
	}
	return f
}

// labels are the labels of every needle found in s.
func (f finder) labels(s string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i+finderKey <= len(s); i++ {
		for _, n := range f[s[i:i+finderKey]] {
			if strings.HasPrefix(s[i:], n[1]) {
				out[n[0]] = true
			}
		}
	}
	return out
}

// find is the first needle found in s: its label and form.
func (f finder) find(s string) (string, string, bool) {
	for i := 0; i+finderKey <= len(s); i++ {
		for _, n := range f[s[i:i+finderKey]] {
			if strings.HasPrefix(s[i:], n[1]) {
				return n[0], n[1], true
			}
		}
	}
	return "", "", false
}

// outputTexts are the texts of a run: its canonical JSON (records with payload, raw, Summary, Body and locator, notes,
// warnings) and every string value as stored (JSON replaces invalid UTF-8, so raw bytes are searched in the values).
func outputTexts(t *testing.T, res parsertest.Result) []string {
	t.Helper()
	all, err := json.Marshal(struct {
		R any
		N map[string]string
		W any
	}{res.Records, res.Notes, res.Warnings})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{string(all)}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []byte:
			out = append(out, string(x))
		case map[string]any:
			for k, e := range x {
				out = append(out, k)
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case []string:
			out = append(out, x...)
		}
	}
	for _, r := range res.Records {
		out = append(out, r.Summary, r.Body, r.Locator, r.SourcePath)
		walk(r.Payload)
	}
	for k, v := range res.Notes {
		out = append(out, k, v)
	}
	for _, w := range res.Warnings {
		out = append(out, w.Locator, w.Reason)
	}
	return out
}

// keyBearing are the variants that hold key material by design (I2): telegram-v181 (the CORE enc_chats rows and secret
// documents), telegram-hostile (enc_chats rows and secret documents whose key and IV bytes lie in the cell (the decode stops at media.document.thumbs, T5)), telegram-layers
// (one row per constructor: a secret-kind field filled with the 4i4-layers chain) and telegram-deleted (live enc_chats
// rows and a deleted secret message with a secret document in its WAL, plan 4I4 Task 14 C1) and telegram-v64 (plan
// 4I4b P7, P8: the file references of a layer-115 photo, its layer-97 location and a layer-113 document) and telegram-v84
// (plan 4I4b V5 to V8: the access_hash int64 credentials of the legacy photo, video, audio and document). Every other
// variant holds none by design, and the test fails when a variant's files say otherwise either way.
func keyBearing() []string {
	return []string{"telegram-v181", "telegram-hostile", "telegram-layers", "telegram-deleted", "telegram-v64", "telegram-v84"}
}

// Spec 9 (call 6), ruling T1: no key material reaches any output of android.telegram. The live pass over every
// committed variant: no record (payload, raw, Summary, Body, locator), note or warning holds any secret-kind value the
// generator fills (keyPatterns), raw, in hex of either case or in standard or URL base64, whole or in any key-bearing
// 8-byte window, nor enc_chats.fprint in decimal or hex. A key-bearing variant whose files hold no pattern, another
// whose files hold one, or a pattern no variant holds fails the test: no subtest asserts nothing unawares. The typed
// pass (android.telegram.recover, recrun.Run with a capturing emitter: every recovered record, unmapped reason, note
// and warning; F15) runs over telegram-deleted and over a Go-built database whose deleted secret rows and enc_chats
// rows (key columns included) lie only in WAL frames; each must recover at least one secret row, so the check is not
// vacuous.
func TestTelegramKeyMaterialNeverEmitted(t *testing.T) {
	ps := keyPatterns(t)
	required, searched := keyInt64s(t)
	f := newFinder(needles(ps, required, searched))
	if name, _, ok := f.find("x" + hex.EncodeToString(ps["chain:4i4-khash"].b[3:11]) + "y"); !ok || name != "chain:4i4-khash" {
		t.Fatal("the finder misses a hex window")
	}
	// the int64 forms are each found: decimal, hex of either case with and without 0x, and the encoded little-endian bytes
	for _, form := range int64Forms(required["i64:9101"]) {
		if name, _, ok := f.find("x" + form + "y"); !ok || !strings.HasPrefix(name, "i64:") {
			t.Fatalf("the finder misses the int64 form %q (found %q)", form, name)
		}
	}
	seen := map[string]bool{}
	for _, v := range variants {
		files := fixtureFiles(t, v)
		held := map[string]bool{}
		for _, role := range []string{"db", "wal", "journal"} {
			for l := range f.labels(string(files[role])) {
				held[l], seen[l] = true, true
			}
		}
		switch bearing := slices.Contains(keyBearing(), v); {
		case bearing && len(held) == 0:
			t.Errorf("%s is listed as key-bearing but its files hold no pattern: its subtest would assert nothing", v)
		case !bearing && len(held) > 0:
			t.Errorf("%s holds %v but is not listed as key-bearing", v, held)
		}
	}
	for name := range ps {
		if !seen[name] {
			t.Errorf("no variant holds the pattern %s", name)
		}
	}
	for name := range required {
		if !seen[name] {
			t.Errorf("no variant holds the int64 credential %s", name)
		}
	}
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			for _, s := range outputTexts(t, parseAt(t, remoteApp, fixtureFiles(t, v))) {
				if name, form, ok := f.find(s); ok {
					t.Errorf("%s: the output holds %s (form %q)", v, name, form)
				}
			}
		})
	}
	typed := map[string]map[string][]byte{"telegram-deleted": fixtureFiles(t, "telegram-deleted"), "go-built-wal": secretRowsInWAL(t)}
	for name, files := range typed {
		t.Run("typed-pass/"+name, func(t *testing.T) {
			held := f.labels(string(files["wal"]))
			if len(held) == 0 {
				t.Fatalf("%s: the WAL holds no key pattern: the check proves nothing", name)
			}
			// final-review ruling F3 (R7): the deleted 4I4b document's int64 credentials lie in telegram-deleted's WAL,
			// so the typed pass's int64 search below is backed
			if name == "telegram-deleted" && !slices.ContainsFunc(slices.Collect(maps.Keys(held)), func(l string) bool { return strings.HasPrefix(l, "i64:") }) {
				t.Errorf("%s: the WAL holds no i64: credential (%v): the typed pass's int64 check proves nothing", name, held)
			}
			res := typedRun(t, files)
			secret := 0
			for _, r := range res.recs {
				if rawOf(r)["secret_chat"] == true {
					secret++
				}
			}
			if secret == 0 {
				t.Fatalf("%s: the typed pass recovered no secret row (%d records): the check proves nothing", name, len(res.recs))
			}
			for _, s := range res.texts(t) {
				if label, form, ok := f.find(s); ok {
					t.Errorf("%s: the typed pass's output holds %s (form %q)", name, label, form)
				}
			}
		})
	}

	// plan 4I4c Task 8, R7 "both parsers": a deleted message whose mention is a full TL_inputUser (access_hash
	// secret_i64(4410), a Stage B credential) lies only in WAL frames; the typed pass recovers it, states the mention and
	// emits none of the credential
	t.Run("typed-pass/go-built-mention-wal", func(t *testing.T) {
		user := slices.Concat(le32(idInputUser), le64(1000009), le64(secretI64Base+4410))
		files := deletedMessageInWAL(t, mentionMessage(user))
		if !f.labels(string(files["wal"]))["i64:4410"] {
			t.Fatalf("the WAL holds no i64:4410 credential (%v): the check proves nothing", f.labels(string(files["wal"])))
		}
		res := typedRun(t, files)
		stated := 0
		for _, r := range res.recs {
			ents, _ := rawOf(r)["entities"].([]any)
			for _, e := range ents {
				if m, _ := e.(map[string]any); m["user"] == "user:1000009" && m["user_access_hash_present"] == true {
					stated++
				}
			}
		}
		if stated != 1 {
			t.Fatalf("the typed pass states %d recovered mentions of the InputUser (%d records): the check proves nothing", stated, len(res.recs))
		}
		for _, s := range res.texts(t) {
			if label, form, ok := f.find(s); ok {
				t.Errorf("the typed pass's output holds %s (form %q)", label, form)
			}
		}
	})
}
