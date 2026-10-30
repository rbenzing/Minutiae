package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Shapes of a typedVariant (4h-R19): where the fixture's files sit in the case.
const (
	shapeAndroidFile = "android-file" // the default: a file of an Android logical acquisition
	shapeIOSFile     = "ios-file"     // a file pulled from a device whose evidence says iOS
	shapeIOSBackup   = "ios-backup"   // the files of an unencrypted iOS backup with its Manifest.db
)

const (
	backupPlistNo = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>IsEncrypted</key><false/></dict></plist>`
)

// iosBackupFile is one file of a test backup: its domain, relative path and bytes.
type iosBackupFile struct {
	domain, rel string
	data        []byte
}

// addIOSBackup captures the files of one unencrypted backup (directory udid) of acquisition acq of device
// "D1", with a Manifest.db that lists each file, a Manifest.plist and a Status.plist. It returns the artifact
// of each file by "domain/rel" and the Manifest.db artifact.
func addIOSBackup(t *testing.T, c *evidence.Case, acq, udid string, files []iosBackupFile) (map[string]evidence.ManifestRecord, evidence.ManifestRecord) {
	t.Helper()
	put := func(rel string, src evidence.Source, data []byte) evidence.ManifestRecord {
		src.DeviceID = "D1"
		rec, err := c.Capture("D1", acq, rel, src, func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	out := map[string]evidence.ManifestRecord{}
	b := sqlitetest.New(sqlitetest.Options{})
	tb := b.CreateTable("Files", "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)")
	for i, f := range files {
		remote := parsertest.BackupRemotePath(udid, f.domain, f.rel)
		out[f.domain+"/"+f.rel] = put("backup/"+udid+"/"+strings.ReplaceAll(f.domain+"-"+f.rel, "/", "_"), evidence.Source{Kind: "backup", RemotePath: remote}, f.data)
		tb.Insert(int64(i+1), parsertest.BackupFileID(f.domain, f.rel), f.domain, f.rel, int64(1), nil)
	}
	manifest := put("backup/"+udid+"/Manifest.db", evidence.Source{Kind: "backup", RemotePath: udid + "/Manifest.db"}, b.Bytes())
	put("backup/"+udid+"/Manifest.plist", evidence.Source{Kind: "backup", RemotePath: udid + "/Manifest.plist"}, []byte(backupPlistNo))
	put("backup/"+udid+"/Status.plist", evidence.Source{Kind: "backup", RemotePath: udid + "/Status.plist"}, []byte("status"))
	return out, manifest
}
