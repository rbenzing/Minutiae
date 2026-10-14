// Package afctest is an in-process fake AFC (Apple File Conduit) device for
// tests. It encodes the wire format itself and imports nothing from this
// module, so a test that runs the client against it is not circular.
package afctest

import (
	"encoding/binary"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	magic      = 0x4141504c36414643 // "CFA6LPAA", little-endian
	headerSize = 40
	maxRequest = 1 << 24
)

// Server is a fake device serving Files read-only.
type Server struct {
	Files     map[string][]byte // absolute path -> content; parent directories are implied
	EmptyDirs []string          // directories that exist and hold nothing
	// Hook sees every request first. handled=true sends raw (nothing when raw
	// is nil) instead of the normal reply.
	Hook func(op uint64, hdr []byte) (raw []byte, handled bool)
	// CloseAfterReply closes the connection after the first reply.
	CloseAfterReply bool

	mu   sync.Mutex
	ops  []uint64
	open map[uint64]*openFile
	next uint64
}

type openFile struct {
	data []byte
	off  int
}

// Pipe starts s on an in-memory connection and returns the client end.
func Pipe(s *Server) io.ReadWriteCloser {
	c, srv := net.Pipe()
	s.open = map[uint64]*openFile{}
	go s.serve(srv)
	return c
}

// Ops returns the operations received so far, in order.
func (s *Server) Ops() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.ops...)
}

// Frame builds one packet: header, header payload, payload.
func Frame(op, num uint64, hdr, payload []byte) []byte {
	b := make([]byte, 0, headerSize+len(hdr)+len(payload))
	for _, v := range []uint64{magic, uint64(headerSize + len(hdr) + len(payload)), uint64(headerSize + len(hdr)), num, op} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	b = append(b, hdr...)
	return append(b, payload...)
}

// Status builds a STATUS packet carrying code.
func Status(num, code uint64) []byte {
	return Frame(0x01, num, binary.LittleEndian.AppendUint64(nil, code), nil)
}

func (s *Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		var h [headerSize]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		entire, this := binary.LittleEndian.Uint64(h[8:]), binary.LittleEndian.Uint64(h[16:])
		num, op := binary.LittleEndian.Uint64(h[24:]), binary.LittleEndian.Uint64(h[32:])
		if this < headerSize || entire < this || entire > maxRequest {
			return
		}
		body := make([]byte, entire-headerSize)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		hp, payload := body[:this-headerSize], body[this-headerSize:]
		s.mu.Lock()
		s.ops = append(s.ops, op)
		s.mu.Unlock()
		reply := s.reply(op, num, hp, payload)
		if s.Hook != nil {
			if raw, handled := s.Hook(op, hp); handled {
				reply = raw
			}
		}
		if reply != nil {
			if _, err := c.Write(reply); err != nil {
				return
			}
		}
		if s.CloseAfterReply {
			return
		}
	}
}

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func (s *Server) children(dir string) ([]string, bool) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]bool{}
	exists := dir == "/"
	for p := range s.Files {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			name, _, _ := strings.Cut(rest, "/")
			seen[name] = true
			exists = true
		}
	}
	for _, d := range s.EmptyDirs {
		if d == dir {
			exists = true
		}
		if rest, ok := strings.CutPrefix(d, prefix); ok {
			name, _, _ := strings.Cut(rest, "/")
			seen[name] = true
			exists = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return append([]string{".", ".."}, names...), exists
}

func pairs(kv ...string) []byte {
	var b []byte
	for _, s := range kv {
		b = append(b, s...)
		b = append(b, 0)
	}
	return b
}

func (s *Server) reply(op, num uint64, hp, _ []byte) []byte {
	const (
		statusNotFound = 8
		statusIsDir    = 9
		statusDenied   = 10
		statusBadArg   = 7
		statusNoOp     = 15
	)
	switch op {
	case 0x03: // READ_DIR
		names, ok := s.children(cstr(hp))
		if !ok {
			return Status(num, statusNotFound)
		}
		return Frame(0x02, num, nil, pairs(names...))
	case 0x0A: // GET_FILE_INFO
		p := cstr(hp)
		if d, ok := s.Files[p]; ok {
			return Frame(0x02, num, nil, pairs("st_size", strconv.Itoa(len(d)), "st_blocks", "8", "st_nlink", "1", "st_ifmt", "S_IFREG"))
		}
		if _, exists := s.children(p); exists {
			return Frame(0x02, num, nil, pairs("st_size", "64", "st_blocks", "0", "st_nlink", "2", "st_ifmt", "S_IFDIR"))
		}
		return Status(num, statusNotFound)
	case 0x0D: // FILE_OPEN
		if len(hp) < 8 {
			return Status(num, statusBadArg)
		}
		if binary.LittleEndian.Uint64(hp) != 1 { // only read-only is served
			return Status(num, statusDenied)
		}
		p := cstr(hp[8:])
		d, ok := s.Files[p]
		if !ok {
			if _, exists := s.children(p); exists {
				return Status(num, statusIsDir)
			}
			return Status(num, statusNotFound)
		}
		s.mu.Lock()
		s.next++
		h := s.next
		s.open[h] = &openFile{data: d}
		s.mu.Unlock()
		return Frame(0x0E, num, binary.LittleEndian.AppendUint64(nil, h), nil)
	case 0x0F: // FILE_READ
		if len(hp) < 16 {
			return Status(num, statusBadArg)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		f := s.open[binary.LittleEndian.Uint64(hp)]
		if f == nil {
			return Status(num, statusBadArg)
		}
		n := int(min(binary.LittleEndian.Uint64(hp[8:]), uint64(len(f.data)-f.off)))
		out := f.data[f.off : f.off+n]
		f.off += n
		return Frame(0x02, num, nil, out) // an empty payload at the end of the file
	case 0x14: // FILE_CLOSE
		if len(hp) >= 8 {
			s.mu.Lock()
			delete(s.open, binary.LittleEndian.Uint64(hp))
			s.mu.Unlock()
		}
		return Status(num, 0)
	}
	return Status(num, statusNoOp)
}
