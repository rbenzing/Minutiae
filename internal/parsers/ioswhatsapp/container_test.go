package ioswhatsapp_test

import (
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	idConsumer = "group.net.whatsapp.WhatsApp.shared"
	idBusiness = "group.net.whatsapp.WhatsAppSMB.shared"
	idKey      = "MCMMetadataIdentifier"
)

func asciiObj(s string) []byte {
	if len(s) < 15 {
		return append([]byte{0x50 | byte(len(s))}, s...)
	}
	return append([]byte{0x5F, 0x10, byte(len(s))}, s...)
}

// bplist builds a binary property list from its objects (object 0 is the root); every offset fits a byte.
func bplist(objs ...[]byte) []byte {
	b := []byte("bplist00")
	offs := make([]byte, len(objs))
	for i, o := range objs {
		offs[i] = byte(len(b))
		b = append(b, o...)
	}
	table := len(b)
	b = append(b, offs...)
	b = append(b, 0, 0, 0, 0, 0, 0, 1, 1, 0, 0, 0, 0, 0, 0, 0, byte(len(objs)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(table))
	return b
}

func binDict(key string, val []byte) []byte {
	return bplist([]byte{0xD1, 1, 2}, asciiObj(key), val)
}

func xmlDict(inner string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>` + inner + `</dict></plist>`)
}

func xmlID(id string) []byte { return xmlDict("<key>" + idKey + "</key><string>" + id + "</string>") }

// withMeta adds a container_meta artifact beside the database of b (the directory of its logical path).
func withMeta(t *testing.T, h *parsertest.Harness, b parsertest.Bundle, data []byte) parsertest.Bundle {
	t.Helper()
	logical := path.Join(path.Dir(b.Primary.Logical), metaName)
	src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: "/private/var/mobile/" + appGroup + "/" + metaName}
	m, err := h.Case.Capture("IOS1", "acq1", "private/var/mobile/"+appGroup+"/"+metaName, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Others["container_meta"] = h.IOSArtifact(m, data, logical)
	return b
}

func notesOf(t *testing.T, v string, meta []byte) map[string]string {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
	if meta != nil {
		b = withMeta(t, h, b, meta)
	}
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatalf("%s: %v", v, res.Err)
	}
	return res.Notes
}

func wantNotes(t *testing.T, label string, got map[string]string, want map[string]string) {
	t.Helper()
	for _, k := range []string{"container", "app", "app_basis", "app_state", "container_identifier", "container_meta_error"} {
		if got[k] != want[k] {
			t.Errorf("%s: note %s = %q, want %q", label, k, got[k], want[k])
		}
	}
}

func TestIOSWhatsAppChatsAppLabelFromStructureOnly(t *testing.T) { // 4I2-R5, spec 6.8
	bd := map[string]string{"container": "backup_domain", "app": "whatsapp", "app_basis": "backup_domain"}
	wantNotes(t, "consumer domain", notesOf(t, "ios-chatstorage", nil), bd)
	// the metadata is never read at a domain, even when it names the other app
	wantNotes(t, "consumer domain, metadata names the business group", notesOf(t, "ios-chatstorage", xmlID(idBusiness)), bd)
	wantNotes(t, "consumer domain, garbage metadata", notesOf(t, "ios-chatstorage", []byte("garbage")), bd)
	wantNotes(t, "business domain", notesOf(t, "ios-chatstorage-business", nil),
		map[string]string{"container": "backup_domain", "app": "whatsapp_business", "app_basis": "backup_domain"})

	ag := func(app string) map[string]string {
		return map[string]string{"container": "app_group", "app": app, "app_basis": "container_metadata"}
	}
	unknown := func(state string, extra ...string) map[string]string {
		m := map[string]string{"container": "app_group", "app": "unknown", "app_state": state}
		if len(extra) == 2 {
			m[extra[0]] = extra[1]
		}
		return m
	}
	const ex = "ios-chatstorage-extract"
	wantNotes(t, "xml consumer", notesOf(t, ex, xmlID(idConsumer)), ag("whatsapp"))
	wantNotes(t, "xml business", notesOf(t, ex, xmlID(idBusiness)), ag("whatsapp_business"))
	wantNotes(t, "binary consumer", notesOf(t, ex, binDict(idKey, asciiObj(idConsumer))), ag("whatsapp"))
	wantNotes(t, "binary business", notesOf(t, ex, binDict(idKey, asciiObj(idBusiness))), ag("whatsapp_business"))

	wantNotes(t, "no metadata", notesOf(t, ex, nil), unknown("no-container-metadata"))
	wantNotes(t, "other identifier", notesOf(t, ex, xmlID("group.com.example.shared")), unknown("metadata-other-identifier", "container_identifier", "group.com.example.shared"))
	wantNotes(t, "similar identifier", notesOf(t, ex, xmlID(idConsumer+".x")), unknown("metadata-other-identifier", "container_identifier", idConsumer+".x"))
	wantNotes(t, "wrong case", notesOf(t, ex, xmlID(strings.ToLower(idConsumer))), unknown("metadata-other-identifier", "container_identifier", strings.ToLower(idConsumer)))
	big := xmlDict("<key>" + idKey + "</key><string>" + idConsumer + "</string><key>pad</key><string>" + strings.Repeat("A", 70<<10) + "</string>")
	wantNotes(t, "70 KiB", notesOf(t, ex, big), unknown("metadata-unreadable", "container_meta_error", "too-large"))
	wantNotes(t, "garbage", notesOf(t, ex, []byte("this is not a property list")), unknown("metadata-unreadable", "container_meta_error", "undecodable"))
	wantNotes(t, "array", notesOf(t, ex, bplist([]byte{0xA1, 1}, []byte{0x10, 5})), unknown("metadata-unreadable", "container_meta_error", "not-a-dictionary"))
	wantNotes(t, "empty plist file", notesOf(t, ex, []byte{}), unknown("metadata-unreadable", "container_meta_error", "undecodable"))
	wantNotes(t, "no key", notesOf(t, ex, xmlDict("<key>Other</key><string>"+idConsumer+"</string>")), unknown("metadata-no-identifier"))
	wantNotes(t, "key is not a string", notesOf(t, ex, binDict(idKey, []byte{0x10, 5})), unknown("metadata-no-identifier"))
	wantNotes(t, "key is not a string (xml)", notesOf(t, ex, xmlDict("<key>"+idKey+"</key><integer>5</integer>")), unknown("metadata-no-identifier"))

	// the database's own text never labels the app: a consumer domain holding Business names stays consumer,
	// an app group with no metadata stays unknown
	for place, c := range map[string]struct {
		domain, rel, logical string
		want                 map[string]string
	}{
		"domain":    {domainConsumer, dbName, "", bd},
		"app group": {"", extractRemote, extractLogical, unknown("no-container-metadata")},
	} {
		h := parsertest.NewHarness(t)
		f := parsertest.Fixture{Name: "smb", Files: map[string][]byte{"db": goStore(okModel, 0)}}
		var b parsertest.Bundle
		if c.domain != "" {
			b = f.BundleIOSBackup(h, c.domain, c.rel)
		} else {
			b = f.BundleIOS(h, c.rel, c.logical)
		}
		p := ioswhatsapp.Parser{}
		res := runLenient(t, h, p, b.Input(h, p, false))
		if res.Err != nil {
			t.Fatalf("%s: %v", place, res.Err)
		}
		wantNotes(t, "WhatsAppSMB in the data, "+place, res.Notes, c.want)
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("device read failed") }

// An I/O failure while reading the container metadata is a failed job, never a label (review M2).
func TestIOSWhatsAppChatsContainerMetaReadFailureFailsTheJob(t *testing.T) {
	for name, r := range map[string]io.ReaderAt{"read error": failingReader{}, "no reader": nil} {
		h := parsertest.NewHarness(t)
		const v = "ios-chatstorage-extract"
		b := withMeta(t, h, bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v), xmlID(idConsumer))
		p := ioswhatsapp.Parser{}
		in := b.Input(h, p, false)
		a := in.Artifacts["container_meta"]
		a.R = r
		in.Artifacts["container_meta"] = a
		res := runLenient(t, h, p, in)
		if res.Err == nil || len(res.Records) != 0 {
			t.Errorf("%s: err %v", name, res.Err)
		}
		if res.Notes["app"] != "" || res.Notes["app_state"] != "" {
			t.Errorf("%s: an app label was written: %v", name, res.Notes)
		}
	}
}
