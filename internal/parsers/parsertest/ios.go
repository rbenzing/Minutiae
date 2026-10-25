package parsertest

import (
	"crypto/sha1" //nolint:gosec // the iOS backup fileID is defined as a SHA-1; it is an identifier here, not a security control
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"testing"

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

// IOSBackupDevice is the device (and backup directory) of iOS backup fixtures.
const IOSBackupDevice = "UDID1"

// BackupFileID is the Manifest.db fileID of a backup file: hex(sha1(domain + "-" + rel)).
func BackupFileID(domain, rel string) string {
	sum := sha1.Sum([]byte(domain + "-" + rel)) //nolint:gosec // the backup format defines the fileID as SHA-1
	return hex.EncodeToString(sum[:])
}

// BackupRemotePath is the Source.RemotePath of a backup file: udid/<first two hex digits>/<fileID>
// (the layout internal/ios/backup.go promotes).
func BackupRemotePath(udid, domain, rel string) string {
	id := BackupFileID(domain, rel)
	return udid + "/" + id[:2] + "/" + id
}

// AddIOSBackupFile captures data as a backup artifact of device udid: Source{Kind "backup",
// DeviceID udid, RemotePath BackupRemotePath(...)} and the case path "backup/" + RemotePath, the
// layout of internal/ios/backup.go.
func AddIOSBackupFile(t testing.TB, c *evidence.Case, acq, udid, domain, rel string, data []byte) evidence.ManifestRecord {
	t.Helper()
	remote := BackupRemotePath(udid, domain, rel)
	src := evidence.Source{Kind: "backup", DeviceID: udid, RemotePath: remote}
	m, err := c.Capture(udid, acq, "backup/"+remote, src, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// BundleIOSBackup stores the fixture as one backup of IOSBackupDevice in acquisition "acq1": the
// database at domain/rel, -wal and -journal beside it (logical +"-wal" / "-journal"), the fixture
// files "images" and "images_wal" in the database's directory as AddressBookImages.sqlitedb(-wal),
// and every extra file at its Domain/RelativePath (a Device other than IOSBackupDevice: acquisition
// "acq-<device>"; an extra with a Role fills that role instead of Others["extraN"]). Each artifact
// is h.IOSArtifact(m, data, "ios:"+domain+"/"+rel). Bundle.ByPath keys an extra by its LOGICAL path
// (4E-R40), so the oracle placeholders @id:, @sha256: and @size: name logical paths.
func (f Fixture) BundleIOSBackup(h *Harness, domain, rel string) Bundle {
	h.T.Helper()
	b, err := f.BuildIOSBackupBundle(h, domain, rel)
	if err != nil {
		h.T.Fatal(err)
	}
	return b
}

// BuildIOSBackupBundle is BundleIOSBackup returning its error: an empty domain or path, of the
// database or of an extra file, is ErrEmptyRemotePath; a role filled twice is an error.
func (f Fixture) BuildIOSBackupBundle(h *Harness, domain, rel string) (Bundle, error) {
	h.T.Helper()
	if domain == "" || rel == "" {
		return Bundle{}, fmt.Errorf("the database: %w", ErrEmptyRemotePath)
	}
	for i, x := range f.Expect.ExtraFiles {
		if x.Domain == "" || x.RelativePath == "" {
			return Bundle{}, fmt.Errorf("extra file %d: %w", i, ErrEmptyRemotePath)
		}
	}
	b := Bundle{Others: map[string]parse.Artifact{}, ByRole: map[string]parse.Artifact{}, ByPath: map[string]parse.Artifact{}}
	add := func(device, d, r string, data []byte) parse.Artifact {
		acq := "acq1"
		if device != IOSBackupDevice {
			acq = "acq-" + device
		}
		return h.IOSArtifact(AddIOSBackupFile(h.T, h.Case, acq, device, d, r, data), data, "ios:"+d+"/"+r)
	}
	b.Primary = add(IOSBackupDevice, domain, rel, f.Files["db"])
	b.ByRole["db"] = b.Primary
	for _, c := range []struct{ role, suffix string }{{"wal", "-wal"}, {"journal", "-journal"}} {
		if data, ok := f.Files[c.role]; ok {
			a := add(IOSBackupDevice, domain, rel+c.suffix, data)
			b.Others[c.role], b.ByRole[c.role] = a, a
		}
	}
	dir := path.Dir(rel)
	for _, c := range []struct{ role, name string }{{"images", "AddressBookImages.sqlitedb"}, {"images_wal", "AddressBookImages.sqlitedb-wal"}} {
		if data, ok := f.Files[c.role]; ok {
			a := add(IOSBackupDevice, domain, path.Join(dir, c.name), data)
			b.Others[c.role], b.ByRole[c.role] = a, a
		}
	}
	for i, x := range f.Expect.ExtraFiles {
		data, err := base64.StdEncoding.DecodeString(x.ContentB64)
		if err != nil {
			return b, fmt.Errorf("extra file %s/%s: %w", x.Domain, x.RelativePath, err)
		}
		device := x.Device
		if device == "" || device == "SER1" {
			device = IOSBackupDevice
		}
		if x.Role != "" {
			if _, taken := b.ByRole[x.Role]; taken {
				return b, fmt.Errorf("extra file %s/%s: the role %q is already filled", x.Domain, x.RelativePath, x.Role)
			}
		}
		a := add(device, x.Domain, x.RelativePath, data)
		if x.Role != "" {
			b.Others[x.Role], b.ByRole[x.Role] = a, a
		} else {
			b.Others["extra"+strconv.Itoa(i)] = a
		}
		b.ByPath[a.Logical] = a
	}
	return b, nil
}
