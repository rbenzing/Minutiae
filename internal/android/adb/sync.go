package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"
)

const syncChunk = 64 * 1024

// SyncEntry is a LIST/STAT result (sync v1: 32-bit size and mtime).
type SyncEntry struct {
	Name  string
	Mode  uint32
	Size  uint32
	MTime time.Time
}

func (e SyncEntry) typ() uint32 { return e.Mode & 0o170000 }

// IsDir reports S_IFDIR.
func (e SyncEntry) IsDir() bool { return e.typ() == 0o040000 }

// IsRegular reports S_IFREG.
func (e SyncEntry) IsRegular() bool { return e.typ() == 0o100000 }

// IsSymlink reports S_IFLNK.
func (e SyncEntry) IsSymlink() bool { return e.typ() == 0o120000 }

// Sync is one sync: session. After any FAIL the device ends the session;
// open a new Sync to continue.
type Sync struct{ c *conn }

// Sync opens a sync session on serial.
func (c *Client) Sync(ctx context.Context, serial string) (*Sync, error) {
	cn, err := c.openService(ctx, serial, "sync:")
	if err != nil {
		return nil, err
	}
	return &Sync{c: cn}, nil
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
	msg := make([]byte, v[0])
	if _, err := io.ReadFull(s.c, msg); err != nil {
		return err
	}
	return &FailError{Msg: string(msg)}
}

// List returns the entries of dir, excluding "." and "..".
func (s *Sync) List(dir string) ([]SyncEntry, error) {
	if err := s.request("LIST", []byte(dir)); err != nil {
		return nil, fmt.Errorf("adb sync list %s: %w", dir, err)
	}
	var out []SyncEntry
	for {
		id, err := s.readID()
		if err != nil {
			return nil, fmt.Errorf("adb sync list %s: %w", dir, err)
		}
		switch id {
		case "DENT":
			v, err := s.readU32s(4) // mode, size, mtime, namelen
			if err != nil {
				return nil, err
			}
			name := make([]byte, v[3])
			if _, err := io.ReadFull(s.c, name); err != nil {
				return nil, err
			}
			if n := string(name); n != "." && n != ".." {
				out = append(out, SyncEntry{Name: n, Mode: v[0], Size: v[1], MTime: time.Unix(int64(v[2]), 0).UTC()})
			}
		case "DONE":
			if _, err := s.readU32s(4); err != nil {
				return nil, err
			}
			return out, nil
		case "FAIL":
			return nil, fmt.Errorf("adb sync list %s: %w", dir, s.readFail())
		default:
			return nil, fmt.Errorf("adb sync list %s: unexpected %q", dir, id)
		}
	}
}

// Stat returns the entry for p; a missing path wraps fs.ErrNotExist.
func (s *Sync) Stat(p string) (SyncEntry, error) {
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
	return SyncEntry{Name: p, Mode: v[0], Size: v[1], MTime: time.Unix(int64(v[2]), 0).UTC()}, nil
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
