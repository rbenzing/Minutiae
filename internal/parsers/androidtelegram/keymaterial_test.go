package androidtelegram_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

// keyPatterns are the fixtures' key material (spec 9, T1): enc_chats g, authkey, fauthkey, khash and data (the
// SHA-256 chains of their seeds), and a secret document's key and IV (the synthetic marker, then the chain bytes, 32
// bytes in all: the chain part is the distinctive pattern; the whole field is searched too).
func keyPatterns() map[string][]byte {
	doc := func(seed string) []byte { return chainBytes(seed, 32-len(syntheticPrefix)) }
	return map[string][]byte{
		"enc_chats.g":        chainBytes("4i4-g", 256),
		"enc_chats.authkey":  chainBytes("4i4-authkey", 256),
		"enc_chats.fauthkey": chainBytes("4i4-fauthkey", 256),
		"enc_chats.khash":    chainBytes("4i4-khash", 32),
		"enc_chats.data":     chainBytes("4i4-encdata", 64),
		"document.key":       doc("4i4-dockey"),
		"document.iv":        doc("4i4-dociv"),
		"document.key.field": append([]byte(syntheticPrefix), doc("4i4-dockey")...),
		"document.iv.field":  append([]byte(syntheticPrefix), doc("4i4-dociv")...),
	}
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

// needles are every form of every pattern and of each of its 8-byte windows, by a label naming the pattern (a
// whole secret field's windows from the first that holds a key byte).
func needles() map[string][]string {
	out := map[string][]string{}
	for name, p := range keyPatterns() {
		out[name] = append(out[name], encodedForms(p)...)
		first := 0
		if strings.HasSuffix(name, ".field") {
			first = len(syntheticPrefix) - 7 // a window holds at least one key byte: the marker alone is not a key
		}
		for i := first; i+8 <= len(p); i++ {
			out[name] = append(out[name], encodedForms(p[i:i+8])...)
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

// Spec 9 (call 6), ruling T1: no key material reaches any output of android.telegram. The live pass over every
// committed variant: no record (payload, raw, Summary, Body, locator), note or warning holds the fixtures' enc_chats
// g, authkey, fauthkey, khash or data, or a secret document's key or IV, raw, in hex of either case or in standard or
// URL base64, whole or in any 8-byte window, nor enc_chats.fprint in decimal or hex. The patterns are first found in
// telegram-v181's files, so the search is never vacuous. The typed pass of telegram-deleted runs once its claims exist
// (Task 14).
func TestTelegramKeyMaterialNeverEmitted(t *testing.T) {
	files := fixtureFiles(t, "telegram-v181")
	held := append(bytes.Clone(files["db"]), files["wal"]...)
	for name, p := range keyPatterns() {
		if !bytes.Contains(held, p) {
			t.Fatalf("telegram-v181 does not hold the pattern %s: the search would prove nothing", name)
		}
	}
	f := newFinder(needles())
	if name, _, ok := f.find("x" + hex.EncodeToString(keyPatterns()["enc_chats.khash"][3:11]) + "y"); !ok || name != "enc_chats.khash" {
		t.Fatal("the finder misses a hex window")
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
	t.Run("typed-pass/telegram-deleted", func(t *testing.T) {
		t.Skip("the typed recovery pass of android.telegram.recover claims its tables in Task 14, which removes this skip " +
			"and searches every recovered record, note and warning for the same patterns")
	})
}
