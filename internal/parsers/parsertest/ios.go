package parsertest

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// IOSArtifact is Artifact for a file acquired from an iOS device: Platform ios and the given
// logical path (for example "ios:HomeDomain/Library/Safari/History.db"), otherwise as Artifact
// (4F-R18). What SourcePath an iOS backup or extract artifact carries in production is the
// host's, not the parser's.
func (h *Harness) IOSArtifact(m evidence.ManifestRecord, data []byte, logical string) parse.Artifact {
	return artifactOf(m, data, parse.PlatformIOS, logical)
}

// BundleIOS stores the fixture's files in h's case as an iOS file acquisition from device IOS1
// (Source{Kind: "file", DeviceID: "IOS1", RemotePath: remotePath}): the database at remotePath
// with logical path logical, the WAL and journal beside it at remotePath+"-wal" / "-journal"
// with logical logical+"-wal" / "-journal". It fails the test on an error.
func (f Fixture) BundleIOS(h *Harness, remotePath, logical string) Bundle {
	h.T.Helper()
	b, err := f.BuildBundleIOS(h, remotePath, logical)
	if err != nil {
		h.T.Fatal(err)
	}
	return b
}

// BuildBundleIOS is BundleIOS returning its error: an empty remote path is ErrEmptyRemotePath, and
// a fixture with extra files is refused (an iOS bundle has no use for them yet; dropping them
// silently would let a check pass vacuously).
func (f Fixture) BuildBundleIOS(h *Harness, remotePath, logical string) (Bundle, error) {
	h.T.Helper()
	if remotePath == "" {
		return Bundle{}, fmt.Errorf("the database: %w", ErrEmptyRemotePath)
	}
	if len(f.Expect.ExtraFiles) > 0 {
		return Bundle{}, errors.New("parsertest: BundleIOS does not support extra files")
	}
	b := Bundle{Others: map[string]parse.Artifact{}, ByRole: map[string]parse.Artifact{}, ByPath: map[string]parse.Artifact{}}
	add := func(path, lg string, data []byte) parse.Artifact {
		src := evidence.Source{Kind: "file", DeviceID: "IOS1", RemotePath: path}
		m, err := h.Case.Capture("IOS1", "acq1", strings.TrimPrefix(path, "/"), src, func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		})
		if err != nil {
			h.T.Fatal(err)
		}
		return h.IOSArtifact(m, data, lg)
	}
	b.Primary = add(remotePath, logical, f.Files["db"])
	b.ByRole["db"] = b.Primary
	for _, c := range []struct{ role, suffix string }{{"wal", "-wal"}, {"journal", "-journal"}} {
		if data, ok := f.Files[c.role]; ok {
			a := add(remotePath+c.suffix, logical+c.suffix, data)
			b.Others[c.role], b.ByRole[c.role] = a, a
		}
	}
	return b, nil
}
