// Package afc is a small, bounds-checked, read-only client for the Apple File
// Conduit service (com.apple.afc). It replaces the go-ios AFC client for
// listing, stat and file reads: every length a device declares is checked
// against a constant before anything is allocated, nothing is read beyond what
// the device actually sends, and only read operations exist. The wire format
// is from memory of public documentation and is cross-checked against the
// go-ios client (afc_wire_test.go); it is not validated on a real device.
package afc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync"
)

const (
	// Magic is "CFA6LPAA" read as a little-endian u64.
	Magic      = uint64(0x4141504c36414643)
	headerSize = 40
	// MaxPacket is the largest entire_length accepted in any reply.
	MaxPacket = 16 << 20
	// MaxRead is the most bytes requested by one read packet.
	MaxRead          = 1 << 20
	maxHeaderPayload = 4096
	maxListEntries   = 1 << 20
	maxStatPairs     = 64
)

const (
	opStatus    uint64 = 0x01
	opReadDir   uint64 = 0x03
	opFileInfo  uint64 = 0x0A
	opFileOpen  uint64 = 0x0D
	opFileRead  uint64 = 0x0F
	opFileClose uint64 = 0x14

	modeReadOnly    uint64 = 1
	statusEndOfData uint64 = 14
)

// ErrProtocol means the device sent a malformed or oversized packet.
var ErrProtocol = errors.New("afc: protocol error")

// StatusError is a non-zero status code returned by the device.
type StatusError struct{ Code uint64 }

func (e *StatusError) Error() string { return fmt.Sprintf("afc: device status %d", e.Code) }

// Is maps the codes of "object not found" (8) and "permission denied" (10).
func (e *StatusError) Is(target error) bool {
	return (e.Code == 8 && target == fs.ErrNotExist) || (e.Code == 10 && target == fs.ErrPermission)
}

// Stat is the result of Client.Stat.
type Stat struct {
	Size   int64
	Type   string // st_ifmt as reported, e.g. S_IFREG
	IsDir  bool
	IsLink bool
}

// Client is one AFC session. Calls are serialized; Close may be called from
// another goroutine to interrupt a blocked call.
type Client struct {
	rw  io.ReadWriteCloser
	mu  sync.Mutex
	num uint64
}

// New wraps a connection to the AFC service.
func New(rw io.ReadWriteCloser) *Client { return &Client{rw: rw} }

// Close closes the connection.
func (c *Client) Close() error { return c.rw.Close() }

type response struct {
	op      uint64
	hdr     []byte
	payload []byte
}

func nul(p string) ([]byte, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return nil, fmt.Errorf("afc: path %q contains a NUL byte", p)
	}
	return append([]byte(p), 0), nil
}

// do sends one request and reads its reply. The caller holds c.mu.
func (c *Client) do(op uint64, hdr []byte) (response, error) {
	c.num++
	b := make([]byte, 0, headerSize+len(hdr))
	for _, v := range []uint64{Magic, uint64(headerSize + len(hdr)), uint64(headerSize + len(hdr)), c.num, op} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	b = append(b, hdr...)
	if _, err := c.rw.Write(b); err != nil {
		return response{}, fmt.Errorf("afc: write: %w", err)
	}
	return c.read()
}

func (c *Client) read() (response, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(c.rw, h[:]); err != nil {
		return response{}, fmt.Errorf("afc: reading header: %w", err)
	}
	if binary.LittleEndian.Uint64(h[0:]) != Magic {
		return response{}, fmt.Errorf("%w: bad magic", ErrProtocol)
	}
	entire, this := binary.LittleEndian.Uint64(h[8:]), binary.LittleEndian.Uint64(h[16:])
	op := binary.LittleEndian.Uint64(h[32:])
	switch {
	case this < headerSize:
		return response{}, fmt.Errorf("%w: this_length %d below the header size", ErrProtocol, this)
	case entire < this:
		return response{}, fmt.Errorf("%w: entire_length %d below this_length %d", ErrProtocol, entire, this)
	case entire > MaxPacket:
		return response{}, fmt.Errorf("%w: entire_length %d exceeds %d", ErrProtocol, entire, MaxPacket)
	case this-headerSize > maxHeaderPayload:
		return response{}, fmt.Errorf("%w: header payload of %d bytes", ErrProtocol, this-headerSize)
	}
	want := int64(entire - headerSize)
	// Read what actually arrives: nothing is allocated from the declared size.
	body, err := io.ReadAll(io.LimitReader(c.rw, want))
	if err != nil {
		return response{}, fmt.Errorf("afc: reading packet: %w", err)
	}
	if int64(len(body)) != want {
		return response{}, fmt.Errorf("afc: packet cut short (%d of %d bytes): %w", len(body), want, io.ErrUnexpectedEOF)
	}
	hl := this - headerSize
	r := response{op: op, hdr: body[:hl], payload: body[hl:]}
	if op == opStatus {
		if len(r.hdr) < 8 {
			return response{}, fmt.Errorf("%w: status packet with a %d-byte header payload", ErrProtocol, len(r.hdr))
		}
		if code := binary.LittleEndian.Uint64(r.hdr); code != 0 {
			return r, &StatusError{Code: code}
		}
	}
	return r, nil
}

// List returns the entry names of dir without "." and "..". A listing with no
// entry at all (not even the two) is a protocol error: an unreadable
// directory is not reported as an empty one.
func (c *Client) List(dir string) ([]string, error) {
	hdr, err := nul(dir)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.do(opReadDir, hdr)
	if err != nil {
		return nil, fmt.Errorf("afc list %s: %w", dir, err)
	}
	if r.op == opStatus {
		return nil, fmt.Errorf("afc list %s: %w: status where a listing was expected", dir, ErrProtocol)
	}
	strs, err := splitNul(r.payload, maxListEntries)
	if err != nil {
		return nil, fmt.Errorf("afc list %s: %w", dir, err)
	}
	if len(strs) == 0 {
		return nil, fmt.Errorf("afc list %s: %w: empty listing without \".\" and \"..\"", dir, ErrProtocol)
	}
	out := make([]string, 0, len(strs))
	for _, n := range strs {
		switch {
		case n == "." || n == "..":
		case n == "" || strings.ContainsRune(n, '/'):
			return nil, fmt.Errorf("afc list %s: %w: entry name %q", dir, ErrProtocol, n)
		default:
			out = append(out, n)
		}
	}
	return out, nil
}

// splitNul splits a payload of NUL-terminated strings; an unterminated tail, or
// more than limit strings, is a protocol error. The NULs are counted before
// anything is split, so the cap is applied before the cost (K16).
func splitNul(b []byte, limit int) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if b[len(b)-1] != 0 {
		return nil, fmt.Errorf("%w: payload not NUL-terminated", ErrProtocol)
	}
	if n := bytes.Count(b, []byte{0}); n > limit {
		return nil, fmt.Errorf("%w: %d strings exceed the limit of %d", ErrProtocol, n, limit)
	}
	return strings.Split(string(b[:len(b)-1]), "\x00"), nil
}

// Stat returns the file information for p. The size and the type are required:
// a reply without them is a protocol error, not a zero size.
func (c *Client) Stat(p string) (Stat, error) {
	hdr, err := nul(p)
	if err != nil {
		return Stat{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.do(opFileInfo, hdr)
	if err != nil {
		return Stat{}, fmt.Errorf("afc stat %s: %w", p, err)
	}
	if r.op == opStatus {
		return Stat{}, fmt.Errorf("afc stat %s: %w: status where file information was expected", p, ErrProtocol)
	}
	strs, err := splitNul(r.payload, 2*maxStatPairs)
	if err != nil {
		return Stat{}, fmt.Errorf("afc stat %s: %w", p, err)
	}
	if len(strs)%2 != 0 || len(strs) > 2*maxStatPairs {
		return Stat{}, fmt.Errorf("afc stat %s: %w: %d strings in the reply", p, ErrProtocol, len(strs))
	}
	seen := map[string]bool{}
	var st Stat
	var haveSize, haveType bool
	for i := 0; i < len(strs); i += 2 {
		k, v := strs[i], strs[i+1]
		if seen[k] {
			return Stat{}, fmt.Errorf("afc stat %s: %w: duplicate key %q", p, ErrProtocol, k)
		}
		seen[k] = true
		switch k {
		case "st_size":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil || n > math.MaxInt64 {
				return Stat{}, fmt.Errorf("afc stat %s: %w: st_size %q", p, ErrProtocol, v)
			}
			st.Size, haveSize = int64(n), true
		case "st_ifmt":
			st.Type, haveType = v, true
			st.IsDir, st.IsLink = v == "S_IFDIR", v == "S_IFLNK"
		}
	}
	if !haveSize || !haveType {
		return Stat{}, fmt.Errorf("afc stat %s: %w: reply lacks st_size or st_ifmt", p, ErrProtocol)
	}
	return st, nil
}

// File is a file opened read-only.
type File struct {
	c      *Client
	handle uint64
	closed bool
}

// Open opens p read-only (mode 1; no other mode exists in this client).
func (c *Client) Open(p string) (*File, error) {
	path, err := nul(p)
	if err != nil {
		return nil, err
	}
	hdr := binary.LittleEndian.AppendUint64(nil, modeReadOnly)
	hdr = append(hdr, path...)
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.do(opFileOpen, hdr)
	if err != nil {
		return nil, fmt.Errorf("afc open %s: %w", p, err)
	}
	if r.op == opStatus || len(r.hdr) < 8 {
		return nil, fmt.Errorf("afc open %s: %w: no file handle in the reply", p, ErrProtocol)
	}
	return &File{c: c, handle: binary.LittleEndian.Uint64(r.hdr)}, nil
}

// Read reads up to len(p) bytes (at most MaxRead per call). An empty data
// reply, or the end-of-data status, is io.EOF; a reply longer than requested
// is a protocol error.
func (f *File) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	want := min(len(p), MaxRead)
	hdr := binary.LittleEndian.AppendUint64(nil, f.handle)
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(want))
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	r, err := f.c.do(opFileRead, hdr)
	var se *StatusError
	if errors.As(err, &se) && se.Code == statusEndOfData {
		return 0, io.EOF
	}
	if err != nil {
		return 0, fmt.Errorf("afc read: %w", err)
	}
	if r.op == opStatus {
		return 0, fmt.Errorf("afc read: %w: status where data was expected", ErrProtocol)
	}
	if len(r.payload) > want {
		return 0, fmt.Errorf("afc read: %w: %d bytes returned for a %d-byte request", ErrProtocol, len(r.payload), want)
	}
	if len(r.payload) == 0 {
		return 0, io.EOF
	}
	return copy(p, r.payload), nil
}

// Close releases the device-side handle. Closing twice is a no-op.
func (f *File) Close() error {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if _, err := f.c.do(opFileClose, binary.LittleEndian.AppendUint64(nil, f.handle)); err != nil {
		return fmt.Errorf("afc close: %w", err)
	}
	return nil
}
