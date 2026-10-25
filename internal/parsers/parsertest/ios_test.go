package parsertest

import (
	"errors"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

const (
	testIOSRemote  = "/private/var/mobile/Library/Safari/History.db"
	testIOSLogical = "ios:HomeDomain/Library/Safari/History.db"
)

func TestBundleIOSMakesAnIOSArtifact(t *testing.T) {
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("main-db"), "wal": []byte("wal-bytes"), "journal": []byte("journal-bytes")}}
	b := fx.BundleIOS(NewHarness(t), testIOSRemote, testIOSLogical)

	for role, want := range map[string]struct{ remote, logical, data string }{
		"db":      {testIOSRemote, testIOSLogical, "main-db"},
		"wal":     {testIOSRemote + "-wal", testIOSLogical + "-wal", "wal-bytes"},
		"journal": {testIOSRemote + "-journal", testIOSLogical + "-journal", "journal-bytes"},
	} {
		a, ok := b.ByRole[role]
		if !ok {
			t.Errorf("%s: no artifact in ByRole", role)
			continue
		}
		if a.Platform != parse.PlatformIOS || a.Logical != want.logical || a.Source.RemotePath != want.remote {
			t.Errorf("%s: platform %q logical %q remote %q, want ios %q %q", role, a.Platform, a.Logical, a.Source.RemotePath, want.logical, want.remote)
		}
		if a.Source.Kind != "file" || a.Source.DeviceID != "IOS1" || a.Acquisition == "" {
			t.Errorf("%s: source %+v acquisition %q", role, a.Source, a.Acquisition)
		}
		got := make([]byte, a.Size)
		if n, err := a.R.ReadAt(got, 0); err != nil && (!errors.Is(err, io.EOF) || n != len(got)) {
			t.Errorf("%s: read: %v", role, err)
		} else if string(got) != want.data {
			t.Errorf("%s: bytes %q, want %q", role, got, want.data)
		}
	}
	if b.Primary.ID != b.ByRole["db"].ID || b.Others["wal"].ID != b.ByRole["wal"].ID || b.Others["journal"].ID != b.ByRole["journal"].ID {
		t.Errorf("Primary and Others do not point at the ByRole artifacts")
	}
	if len(b.Others) != 2 {
		t.Errorf("Others holds %d artifacts, want wal and journal", len(b.Others))
	}
}

func TestBundleIOSWithoutCompanionsHasNone(t *testing.T) {
	b := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}}.BundleIOS(NewHarness(t), testIOSRemote, testIOSLogical)
	if len(b.Others) != 0 || len(b.ByRole) != 1 {
		t.Errorf("Others %v ByRole %v, want only the database", b.Others, b.ByRole)
	}
}

func TestBuildBundleIOSRefusesAnEmptyRemotePathAndExtraFiles(t *testing.T) {
	fx := Fixture{Name: "x", Files: map[string][]byte{"db": []byte("d")}}
	if _, err := fx.BuildBundleIOS(NewHarness(t), "", testIOSLogical); !errors.Is(err, ErrEmptyRemotePath) {
		t.Errorf("empty remote path: err = %v, want ErrEmptyRemotePath", err)
	}
	fx.Expect.ExtraFiles = []ExtraFile{{RemotePath: "/x", ContentB64: "AA=="}}
	if _, err := fx.BuildBundleIOS(NewHarness(t), testIOSRemote, testIOSLogical); err == nil {
		t.Error("extra files were silently dropped")
	}
}

func TestIOSArtifactIsAnIOSArtifactOfTheManifestRecord(t *testing.T) {
	h := NewHarness(t)
	m := AddAndroidFile(t, h.Case, "acq1", "/private/var/x.db", []byte("zz"))
	a := h.IOSArtifact(m, []byte("zz"), "ios:HomeDomain/x.db")
	if a.Platform != parse.PlatformIOS || a.Logical != "ios:HomeDomain/x.db" || a.ID != m.ID || a.SHA256 != m.SHA256 || a.Size != 2 {
		t.Errorf("artifact %+v", a)
	}
}
