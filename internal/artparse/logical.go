package artparse

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// deviceFileNamer names a file pulled from a device. The platform is the
// device's, known from case evidence (Env), never from the shape of the path.
type deviceFileNamer struct{}

func (deviceFileNamer) Name() string { return "device-file" }

func (deviceFileNamer) Logical(m evidence.ManifestRecord, env Env) (Logical, bool) {
	if m.Source.Kind != "file" {
		return Logical{}, false
	}
	switch p := env.DevicePlatform[m.Source.DeviceID]; p {
	case parse.PlatformAndroid:
		if np, ok := NormalizeAndroidPath(m.Source.RemotePath); ok {
			return Logical{Path: "android:" + np, Platform: p, Namer: "device-file"}, true
		}
	case parse.PlatformIOS:
		if np, ok := normalizeIOSRelative(strings.TrimPrefix(m.Source.RemotePath, "/")); ok {
			return Logical{Path: "ios:MediaDomain/" + np, Platform: p, Namer: "device-file"}, true
		}
	}
	return Logical{}, false
}

// extractNamer names a file extracted from an image: the platform comes from
// the filesystem type.
type extractNamer struct{}

func (extractNamer) Name() string { return "extract" }

const iosMobilePrefix = "/private/var/mobile/"

func (extractNamer) Logical(m evidence.ManifestRecord, _ Env) (Logical, bool) {
	d := m.Source.Derived
	if m.Source.Kind != "extract" || d == nil {
		return Logical{}, false
	}
	np, ok := NormalizeAndroidPath(d.FSPath)
	if !ok {
		return Logical{}, false
	}
	switch d.FSType {
	case "ext2", "ext3", "ext4", "f2fs", "fat12", "fat16", "fat32", "exfat":
		return Logical{Path: "android:" + np, Platform: parse.PlatformAndroid, Namer: "extract"}, true
	case "apfs", "hfsplus", "hfsx":
		if rest, found := strings.CutPrefix(np, iosMobilePrefix); found {
			if rel, ok := normalizeIOSRelative(rest); ok {
				return Logical{Path: "ios:HomeDomain/" + rel, Platform: parse.PlatformIOS, Namer: "extract"}, true
			}
		}
	}
	return Logical{}, false
}
