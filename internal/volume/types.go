package volume

import "fmt"

// gptTypeNames maps lower-case canonical GPT partition type GUIDs to names.
// Android partitions are identified by the GPT Name field, not by type.
var gptTypeNames = map[string]string{
	"0fc63daf-8483-4772-8e79-3d69d8477de4": "Linux filesystem",
	"c12a7328-f81f-11d2-ba4b-00a0c93ec93b": "EFI System",
	"ebd0a0a2-b9e5-4433-87c0-68b6b72699c7": "Microsoft basic data",
	"7c3457ef-0000-11aa-aa11-00306543ecac": "Apple APFS",
	"48465300-0000-11aa-aa11-00306543ecac": "Apple HFS+",
}

var mbrTypeNames = map[byte]string{
	0x01: "FAT12",
	0x04: "FAT16",
	0x06: "FAT16",
	0x0e: "FAT16",
	0x0b: "FAT32",
	0x0c: "FAT32",
	0x07: "NTFS/exFAT",
	0x83: "Linux",
	0xee: "GPT protective",
}

func mbrTypeString(t byte) string { return fmt.Sprintf("0x%02x", t) }

func isExtendedType(t byte) bool { return t == 0x05 || t == 0x0f || t == 0x85 }
