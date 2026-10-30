// Package iosbackup reads the two files of an iOS backup that name its other files: the Files
// table of Manifest.db and the IsEncrypted key of Manifest.plist.
//
// A backup stores every file under a hashed name (<dir>/<aa>/<fileID>); only Manifest.db says
// which domain and relative path a file ID stands for. ReadFiles scans that table through
// decode/sqlitedb (the live view: a WAL or journal of the database is applied) and keeps only the
// rows of the file IDs the caller asks for. Files.Lookup then answers for each of them with a
// State; a lookup that cannot be decided says so (Undecided) and never answers "not listed".
// ReadEncryption reads the encryption flag with the strict plist decoder; anything it cannot
// decide is EncUnknown with a reason, never "not encrypted".
//
// Nothing here repairs a value: a domain or a relative path that is not already canonical is a
// BadEntry, never normalised. The package imports decode/sqlitedb and decode/plist only, works on
// bytes and values, and can never write to a case.
package iosbackup
