// Package mb2 implements the host side of Apple's com.apple.mobilebackup2
// service over the DeviceLink protocol. It imports nothing from Minutiae.
package mb2

import (
	"encoding/binary"
	"fmt"
	"io"

	"howett.net/plist"
)

// File-transfer block codes.
const (
	CodeSuccess     byte = 0x00
	CodeErrorLocal  byte = 0x06
	CodeErrorRemote byte = 0x0b
	CodeFileData    byte = 0x0c
)

// EmptyParameter is DeviceLink's placeholder for an empty string argument.
const EmptyParameter = "___EmptyParameterString___"

const (
	maxMessage = 64 << 20
	maxName    = 4096
	chunkSize  = 64 << 10
)

// Codec frames DeviceLink messages: 4-byte big-endian length + binary plist array.
type Codec struct{ rw io.ReadWriter }

// NewCodec wraps rw.
func NewCodec(rw io.ReadWriter) Codec { return Codec{rw: rw} }

// Send encodes msg as a binary plist array.
func (c Codec) Send(msg []any) error {
	b, err := plist.Marshal(msg, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("mb2: encode: %w", err)
	}
	frame := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b)))
	_, err = c.rw.Write(append(frame, b...))
	return err
}

// Recv reads one message; it must be a non-empty array.
func (c Codec) Recv() ([]any, error) {
	n, err := ReadU32(c.rw)
	if err != nil {
		return nil, err
	}
	if n > maxMessage {
		return nil, fmt.Errorf("mb2: message of %d bytes exceeds limit", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.rw, b); err != nil {
		return nil, err
	}
	if err := checkBinaryPlist(b); err != nil {
		return nil, err
	}
	v, err := decodePlist(b)
	if err != nil {
		return nil, fmt.Errorf("mb2: decode: %w", err)
	}
	msg, ok := v.([]any)
	if !ok || len(msg) == 0 {
		return nil, fmt.Errorf("mb2: message is not a non-empty array (%T)", v)
	}
	return msg, nil
}

// unmarshal is the plist decoder; a variable so tests can inject a failing one.
var unmarshal = plist.Unmarshal

// decodePlist decodes b, converting a decoder panic on hostile input into an error.
func decodePlist(b []byte) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("mobilebackup2: malformed plist (%v)", r)
		}
	}()
	_, err = unmarshal(b, &v)
	return v, err
}

// Name returns msg[0] as a string.
func Name(msg []any) string {
	if len(msg) == 0 {
		return ""
	}
	s, _ := msg[0].(string)
	return s
}

// ToInt converts plist integer/real values.
func ToInt(v any) (int64, bool) {
	switch x := v.(type) {
	case uint64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

func arg(msg []any, i int) any {
	if i < len(msg) {
		return msg[i]
	}
	return nil
}

// WriteU32 writes a big-endian uint32.
func WriteU32(w io.Writer, v uint32) error {
	_, err := w.Write(binary.BigEndian.AppendUint32(nil, v))
	return err
}

// ReadU32 reads a big-endian uint32.
func ReadU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

// ReadByte reads one byte.
func ReadByte(r io.Reader) (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}

// WriteName writes a length-prefixed name.
func WriteName(w io.Writer, s string) error {
	if err := WriteU32(w, uint32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

// ReadName reads a length-prefixed name; length 0 yields "" (end of list).
func ReadName(r io.Reader) (string, error) {
	n, err := ReadU32(r)
	if err != nil || n == 0 {
		return "", err
	}
	if n > maxName {
		return "", fmt.Errorf("mb2: name of %d bytes exceeds limit", n)
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}

// WriteBlock writes one transfer block.
func WriteBlock(w io.Writer, code byte, payload []byte) error {
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 5+len(payload)), uint32(len(payload)+1))
	b = append(b, code)
	_, err := w.Write(append(b, payload...))
	return err
}
