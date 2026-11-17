package wajid

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The typed refusals of Parse.
var (
	ErrNoServer    = errors.New("wajid: no @ in the JID")
	ErrEmptyUser   = errors.New("wajid: empty user part")
	ErrEmptyServer = errors.New("wajid: empty server")
	ErrBadSuffix   = errors.New("wajid: agent or device suffix is not a decimal in range")
	ErrNotText     = errors.New("wajid: not valid UTF-8 text without NUL")
)

// Class is the kind of address a server names.
type Class string

// The classes of a server.
const (
	ClassUser       Class = "user"
	ClassGroup      Class = "group"
	ClassBroadcast  Class = "broadcast"
	ClassStatus     Class = "status"
	ClassLID        Class = "lid"
	ClassNewsletter Class = "newsletter"
	ClassOther      Class = "other"
)

// JID is a JID split as stored.
type JID struct {
	RawUser string  // the user part as stored, suffixes included
	User    string  // RawUser without the .agent and :device suffixes
	Server  string  // the server as stored
	Agent   *uint8  // nil when absent
	Device  *uint16 // nil when absent
	Class   Class
}

// ServerClass names a server, its class and the research sources that establish it.
type ServerClass struct {
	Server  string
	Class   Class
	Sources []string
}

// ServerClasses returns the servers Parse classifies, each with the research sources that
// establish it (research §7.11). A server not listed is ClassOther. The status rule is
// status@broadcast, which S15, S11, S6 and S1 establish. A fresh slice is built per call, so
// the package holds no mutable table.
func ServerClasses() []ServerClass {
	return []ServerClass{
		{"s.whatsapp.net", ClassUser, []string{"S9", "S15", "S11", "S3"}},
		{"g.us", ClassGroup, []string{"S15", "S11", "S3", "S9"}},
		{"broadcast", ClassBroadcast, []string{"S15", "S11"}},
		{"lid", ClassLID, []string{"S15", "S3", "S11", "S4", "S7"}},
		{"newsletter", ClassNewsletter, []string{"S15", "S11", "S4", "S3"}},
		{"status@broadcast", ClassStatus, []string{"S15", "S11", "S6", "S1"}},
	}
}

// Parse splits s as stored: at the last '@', then the optional ":device" and ".agent"
// suffixes of the user part. User, RawUser and Server are substrings of s. The server is
// classified by an exact, case-sensitive comparison; nothing is folded, completed or repaired.
func Parse(s string) (JID, error) {
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return JID{}, ErrNotText
	}
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return JID{}, ErrNoServer
	}
	raw, server := s[:at], s[at+1:]
	if raw == "" {
		return JID{}, ErrEmptyUser
	}
	if server == "" {
		return JID{}, ErrEmptyServer
	}
	j := JID{RawUser: raw, User: raw, Server: server}
	if c := strings.LastIndexByte(raw, ':'); c >= 0 {
		d, err := strconv.ParseUint(raw[c+1:], 10, 16)
		if err != nil {
			return JID{}, ErrBadSuffix
		}
		dev := uint16(d)
		j.Device = &dev
		j.User = raw[:c]
		if dot := strings.LastIndexByte(j.User, '.'); dot >= 0 {
			a, err := strconv.ParseUint(j.User[dot+1:], 10, 8)
			if err != nil {
				return JID{}, ErrBadSuffix
			}
			ag := uint8(a)
			j.Agent = &ag
			j.User = j.User[:dot]
		}
		if j.User == "" {
			return JID{}, ErrEmptyUser
		}
	}
	switch server {
	case "s.whatsapp.net":
		j.Class = ClassUser
	case "g.us":
		j.Class = ClassGroup
	case "broadcast":
		j.Class = ClassBroadcast
		if j.User == "status" {
			j.Class = ClassStatus
		}
	case "lid":
		j.Class = ClassLID
	case "newsletter":
		j.Class = ClassNewsletter
	default:
		j.Class = ClassOther
	}
	return j, nil
}
