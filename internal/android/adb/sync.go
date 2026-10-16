package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strings"
	"time"
)

const (
	syncChunk = 64 * 1024
	// maxFailMsg and maxDentName bound what a hostile device can make us
	// allocate: lengths above them are protocol errors, checked before any
	// buffer is allocated.
	maxFailMsg  = 64 * 1024
	maxDentName = 4096
)

// DefaultMaxListEntries bounds a LIST when the caller gives no limit.
const DefaultMaxListEntries = 1_000_000

// maxListNameBytes bounds the entry-name bytes one LIST may deliver (a count
// cap alone would let 1,000,000 names of 4096 bytes cost 4 GiB).
const maxListNameBytes = 64 << 20

// maxListEntriesCeiling clamps a caller-supplied limit so limit+2 cannot overflow.
const maxListEntriesCeiling = 1 << 30

// ErrListLimit means a directory listing had more entries than the limit. The
// device still has frames pending, so the session must be closed.
var ErrListLimit = errors.New("adb sync: directory listing exceeds the entry limit")

// ListResult is what a LIST returned.
type ListResult struct {
	Entries     []SyncEntry // without "." and ".."
	Raw         int         // entry frames received, "." and ".." included
	V2          bool
	DirErrno    uint32
	EntryErrors []EntryError
}

// SyncEntry is a LIST/STAT result. Sizes are 64-bit in sync v2 (widened from
// 32-bit, and possibly truncated by the device, in v1).
type SyncEntry struct {
	Name string
	Mode uint32
	Size int64
	// MTime is the device's modification time; MTimeUnknown is set (and MTime
	// zero) when v2 reports a time that is not a plausible date.
	MTime        time.Time
	MTimeUnknown bool
	// V2 is true when the session that produced the entry spoke sync v2, whose
	// sizes are 64-bit; a v1 size may be 32-bit truncated. The version is the
	// listing session's, not the session current when the file is pulled.
	V2 bool
}

func (e SyncEntry) typ() uint32 { return e.Mode & 0o170000 }

// IsDir reports S_IFDIR.
func (e SyncEntry) IsDir() bool { return e.typ() == 0o040000 }

// IsRegular reports S_IFREG.
func (e SyncEntry) IsRegular() bool { return e.typ() == 0o100000 }

// IsSymlink reports S_IFLNK.
func (e SyncEntry) IsSymlink() bool { return e.typ() == 0o120000 }

// EntryError is a v2 entry the device could not stat.
type EntryError struct {
	Name  string
	Errno uint32
}

// ErrnoError is an errno the device reported.
type ErrnoError struct{ Errno uint32 }

func (e *ErrnoError) Error() string { return fmt.Sprintf("adb: device reported errno %d", e.Errno) }

// Is maps ENOENT (2) and EPERM/EACCES (1, 13).
func (e *ErrnoError) Is(target error) bool {
	return (e.Errno == 2 && target == fs.ErrNotExist) || ((e.Errno == 1 || e.Errno == 13) && target == fs.ErrPermission)
}

// SyncMode selects the sync protocol version of a session.
type SyncMode int

const (
	// SyncAuto uses v2 when the device advertises both ls_v2 and stat_v2 and v1 otherwise.
	SyncAuto SyncMode = iota
	// SyncV1 forces v1.
	SyncV1
	// SyncV2 forces v2; the device must support it.
	SyncV2
)

// Sync is one sync: session. After any FAIL the device ends the session;
// open a new Sync to continue.
type Sync struct {
	c        *conn
	ver      int
	fallback string
}

// Version is 2 when the session speaks sync v2 and 1 otherwise.
func (s *Sync) Version() int { return s.ver }

// FallbackReason says why the session speaks v1 ("" for v2).
func (s *Sync) FallbackReason() string { return s.fallback }

// Features returns the features the adb server reports for the device
// (host-serial:<serial>:features).
func (c *Client) Features(ctx context.Context, serial string) ([]string, error) {
	cn, err := c.hostRequest(ctx, "host-serial:"+serial+":features")
	if err != nil {
		return nil, err
	}
	defer func() { _ = cn.Close() }()
	body, err := readLenString(cn)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range strings.Split(body, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

// Sync opens a sync session on serial, negotiating v2 when the device offers it.
func (c *Client) Sync(ctx context.Context, serial string) (*Sync, error) {
	return c.SyncWith(ctx, serial, SyncAuto)
}

// SyncWith opens a sync session in the given mode.
func (c *Client) SyncWith(ctx context.Context, serial string, mode SyncMode) (*Sync, error) {
	ver, why := 1, "v1 requested"
	switch mode {
	case SyncV2:
		ver, why = 2, ""
	case SyncAuto:
		feats, err := c.Features(ctx, serial)
		var fe *FailError
		switch {
		case err == nil:
			ver, why = chooseVersion(feats)
		case errors.As(err, &fe):
			ver, why = 1, "feature query refused: "+fe.Msg
		default:
			return nil, err
		}
	}
	cn, err := c.openService(ctx, serial, "sync:")
	if err != nil {
		return nil, err
	}
	return &Sync{c: cn, ver: ver, fallback: why}, nil
}

// chooseVersion needs both ls_v2 and stat_v2.
func chooseVersion(feats []string) (int, string) {
	var ls, st bool
	for _, f := range feats {
		ls = ls || f == "ls_v2"
		st = st || f == "stat_v2"
	}
	switch {
	case ls && st:
		return 2, ""
	case !ls && !st:
		return 1, "the device advertises neither ls_v2 nor stat_v2"
	case !ls:
		return 1, "the device does not advertise ls_v2"
	default:
		return 1, "the device does not advertise stat_v2"
	}
}

const (
	v2DentRest = 72 // bytes after the id of DNT2 and of the final DONE frame
	v2StatRest = 68 // bytes after the id of STA2
	// maxPlausibleUnix is 9999-12-31T23:59:59Z; a v2 time outside 0..this is unknown.
	maxPlausibleUnix = 253402300799
)

func (s *Sync) readN(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.c, b); err != nil {
		return nil, err
	}
	return b, nil
}

// v2Fields are the fields of a DNT2/STA2 frame after the id.
type v2Fields struct {
	errno, mode uint32
	size        uint64
	mtime       int64
}

func parseV2(h []byte) v2Fields {
	return v2Fields{
		errno: binary.LittleEndian.Uint32(h[0:]),
		mode:  binary.LittleEndian.Uint32(h[20:]),
		size:  binary.LittleEndian.Uint64(h[36:]),
		mtime: int64(binary.LittleEndian.Uint64(h[52:])),
	}
}

// entryV2 turns the fields into an entry; a size over MaxInt64 is a protocol error.
func entryV2(name string, f v2Fields) (SyncEntry, error) {
	if f.size > math.MaxInt64 {
		return SyncEntry{}, fmt.Errorf("%w: size %d exceeds int64", ErrProtocol, f.size)
	}
	e := SyncEntry{Name: name, Mode: f.mode, Size: int64(f.size), V2: true}
	if f.mtime < 0 || f.mtime > maxPlausibleUnix {
		e.MTimeUnknown = true
	} else {
		e.MTime = time.Unix(f.mtime, 0).UTC()
	}
	return e, nil
}

func (s *Sync) listV2(dir string, limit int) (ListResult, error) {
	res := ListResult{V2: true}
	nameBytes := 0
	if err := s.request("LIS2", []byte(dir)); err != nil {
		return res, fmt.Errorf("adb sync list %s: %w", dir, err)
	}
	for {
		id, err := s.readID()
		if err != nil {
			return res, fmt.Errorf("adb sync list %s: %w", dir, err)
		}
		switch id {
		case "DNT2":
			h, err := s.readN(v2DentRest)
			if err != nil {
				return res, err
			}
			namelen := binary.LittleEndian.Uint32(h[68:])
			if namelen > maxDentName {
				return res, fmt.Errorf("adb sync list %s: %w: DNT2 name length %d exceeds %d", dir, ErrProtocol, namelen, maxDentName)
			}
			name := make([]byte, namelen)
			if _, err := io.ReadFull(s.c, name); err != nil {
				return res, err
			}
			res.Raw++
			nameBytes += len(name)
			if nameBytes > maxListNameBytes {
				return res, fmt.Errorf("adb sync list %s: %w (name bytes over %d)", dir, ErrListLimit, maxListNameBytes)
			}
			if res.Raw > limit+2 {
				return res, fmt.Errorf("adb sync list %s: %w (limit %d)", dir, ErrListLimit, limit)
			}
			n := string(name)
			if n == "." || n == ".." {
				continue
			}
			if len(res.Entries)+len(res.EntryErrors) == limit {
				return res, fmt.Errorf("adb sync list %s: %w (limit %d)", dir, ErrListLimit, limit)
			}
			f := parseV2(h)
			if f.errno != 0 {
				res.EntryErrors = append(res.EntryErrors, EntryError{Name: n, Errno: f.errno})
				continue
			}
			e, err := entryV2(n, f)
			if err != nil {
				return res, fmt.Errorf("adb sync list %s: %w", dir, err)
			}
			res.Entries = append(res.Entries, e)
		case "DONE":
			h, err := s.readN(v2DentRest)
			if err != nil {
				return res, err
			}
			res.DirErrno = binary.LittleEndian.Uint32(h[0:]) // assumed: an errno for the directory itself
			return res, nil
		case "FAIL":
			return res, fmt.Errorf("adb sync list %s: %w", dir, s.readFail())
		default:
			return res, fmt.Errorf("adb sync list %s: unexpected %q", dir, id)
		}
	}
}

func (s *Sync) statV2(p string) (SyncEntry, error) {
	if err := s.request("STA2", []byte(p)); err != nil {
		return SyncEntry{}, err
	}
	id, err := s.readID()
	if err != nil {
		return SyncEntry{}, err
	}
	switch id {
	case "FAIL":
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: %w", p, s.readFail())
	case "STA2":
	default:
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: unexpected %q", p, id)
	}
	h, err := s.readN(v2StatRest)
	if err != nil {
		return SyncEntry{}, err
	}
	f := parseV2(h)
	if f.errno != 0 {
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: %w", p, &ErrnoError{Errno: f.errno})
	}
	e, err := entryV2(p, f)
	if err != nil {
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: %w", p, err)
	}
	return e, nil
}

func (s *Sync) request(id string, arg []byte) error {
	b := make([]byte, 8, 8+len(arg))
	copy(b, id)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(arg)))
	_, err := s.c.Write(append(b, arg...))
	return err
}

func (s *Sync) readID() (string, error) {
	var b [4]byte
	_, err := io.ReadFull(s.c, b[:])
	return string(b[:]), err
}

func (s *Sync) readU32s(n int) ([]uint32, error) {
	b := make([]byte, 4*n)
	if _, err := io.ReadFull(s.c, b); err != nil {
		return nil, err
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return out, nil
}

func (s *Sync) readFail() error {
	v, err := s.readU32s(1)
	if err != nil {
		return err
	}
	if v[0] > maxFailMsg {
		return fmt.Errorf("%w: FAIL message length %d exceeds %d", ErrProtocol, v[0], maxFailMsg)
	}
	msg := make([]byte, v[0])
	if _, err := io.ReadFull(s.c, msg); err != nil {
		return err
	}
	return &FailError{Msg: string(msg)}
}

// List returns the entries of dir, excluding "." and "..", reading at most
// DefaultMaxListEntries. Entries returned together with a non-nil error are
// PARTIAL: they must never be used as the whole listing. After ErrListLimit the
// device still has frames pending and the sync session is unusable.
func (s *Sync) List(dir string) ([]SyncEntry, error) {
	res, err := s.ListN(dir, DefaultMaxListEntries)
	return res.Entries, err
}

// ListN is List with a limit on the entries read. More than limit entries (or
// more than limit+2 frames: a real directory has one "." and one "..") is
// ErrListLimit with the first limit entries in the result. Entries returned
// with a non-nil error are PARTIAL and must never be used as the whole
// listing; after ErrListLimit the sync session is unusable. A limit of zero or
// less means DefaultMaxListEntries; a huge limit is clamped.
func (s *Sync) ListN(dir string, limit int) (ListResult, error) {
	if limit <= 0 {
		limit = DefaultMaxListEntries
	}
	limit = min(limit, maxListEntriesCeiling) // limit+2 below must not overflow
	if s.ver == 2 {
		return s.listV2(dir, limit)
	}
	var res ListResult
	nameBytes := 0
	if err := s.request("LIST", []byte(dir)); err != nil {
		return res, fmt.Errorf("adb sync list %s: %w", dir, err)
	}
	for {
		id, err := s.readID()
		if err != nil {
			return res, fmt.Errorf("adb sync list %s: %w", dir, err)
		}
		switch id {
		case "DENT":
			v, err := s.readU32s(4) // mode, size, mtime, namelen
			if err != nil {
				return res, err
			}
			if v[3] > maxDentName {
				return res, fmt.Errorf("adb sync list %s: %w: DENT name length %d exceeds %d", dir, ErrProtocol, v[3], maxDentName)
			}
			name := make([]byte, v[3])
			if _, err := io.ReadFull(s.c, name); err != nil {
				return res, err
			}
			res.Raw++
			nameBytes += len(name)
			if nameBytes > maxListNameBytes {
				return res, fmt.Errorf("adb sync list %s: %w (name bytes over %d)", dir, ErrListLimit, maxListNameBytes)
			}
			if res.Raw > limit+2 {
				return res, fmt.Errorf("adb sync list %s: %w (limit %d)", dir, ErrListLimit, limit)
			}
			if n := string(name); n != "." && n != ".." {
				if len(res.Entries) == limit {
					return res, fmt.Errorf("adb sync list %s: %w (limit %d)", dir, ErrListLimit, limit)
				}
				res.Entries = append(res.Entries, SyncEntry{Name: n, Mode: v[0], Size: int64(v[1]), MTime: time.Unix(int64(v[2]), 0).UTC()})
			}
		case "DONE":
			if _, err := s.readU32s(4); err != nil {
				return res, err
			}
			return res, nil
		case "FAIL":
			return res, fmt.Errorf("adb sync list %s: %w", dir, s.readFail())
		default:
			return res, fmt.Errorf("adb sync list %s: unexpected %q", dir, id)
		}
	}
}

// Stat returns the entry for p; a missing path wraps fs.ErrNotExist.
func (s *Sync) Stat(p string) (SyncEntry, error) {
	if s.ver == 2 {
		return s.statV2(p)
	}
	return s.statV1(p)
}

func (s *Sync) statV1(p string) (SyncEntry, error) {
	if err := s.request("STAT", []byte(p)); err != nil {
		return SyncEntry{}, err
	}
	id, err := s.readID()
	if err != nil {
		return SyncEntry{}, err
	}
	if id != "STAT" {
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: unexpected %q", p, id)
	}
	v, err := s.readU32s(3)
	if err != nil {
		return SyncEntry{}, err
	}
	if v[0] == 0 {
		return SyncEntry{}, fmt.Errorf("adb sync stat %s: %w", p, fs.ErrNotExist)
	}
	return SyncEntry{Name: p, Mode: v[0], Size: int64(v[1]), MTime: time.Unix(int64(v[2]), 0).UTC()}, nil
}

// Recv streams the remote file p into w.
func (s *Sync) Recv(p string, w io.Writer) (int64, error) {
	if err := s.request("RECV", []byte(p)); err != nil {
		return 0, fmt.Errorf("adb sync recv %s: %w", p, err)
	}
	var total int64
	for {
		id, err := s.readID()
		if err != nil {
			return total, fmt.Errorf("adb sync recv %s: %w", p, err)
		}
		switch id {
		case "DATA":
			v, err := s.readU32s(1)
			if err != nil {
				return total, err
			}
			if v[0] > syncChunk {
				return total, fmt.Errorf("adb sync recv %s: oversized chunk %d", p, v[0])
			}
			n, err := io.CopyN(w, s.c, int64(v[0]))
			total += n
			if err != nil {
				return total, fmt.Errorf("adb sync recv %s: %w", p, err)
			}
		case "DONE":
			_, err := s.readU32s(1)
			return total, err
		case "FAIL":
			return total, fmt.Errorf("adb sync recv %s: %w", p, s.readFail())
		default:
			return total, fmt.Errorf("adb sync recv %s: unexpected %q", p, id)
		}
	}
}

// Send writes r to remote path p with the given st_mode and mtime.
func (s *Sync) Send(p string, mode uint32, mtime time.Time, r io.Reader) error {
	if err := s.request("SEND", []byte(fmt.Sprintf("%s,%d", p, mode))); err != nil {
		return fmt.Errorf("adb sync send %s: %w", p, err)
	}
	buf := make([]byte, syncChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := s.request("DATA", buf[:n]); werr != nil {
				return fmt.Errorf("adb sync send %s: %w", p, werr)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("adb sync send %s: %w", p, err)
		}
	}
	done := binary.LittleEndian.AppendUint32([]byte("DONE"), uint32(mtime.Unix()))
	if _, err := s.c.Write(done); err != nil {
		return err
	}
	id, err := s.readID()
	if err != nil {
		return err
	}
	switch id {
	case "OKAY":
		_, err := s.readU32s(1)
		return err
	case "FAIL":
		return fmt.Errorf("adb sync send %s: %w", p, s.readFail())
	default:
		return fmt.Errorf("adb sync send %s: unexpected %q", p, id)
	}
}

// Close ends the session.
func (s *Sync) Close() error {
	_ = s.request("QUIT", nil)
	return s.c.Close()
}
