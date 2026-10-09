// Package mb2 implements the host side of Apple's com.apple.mobilebackup2
// service over the DeviceLink protocol. Its only Minutiae import is the pure leaf decoder
// internal/decode/plist, which validates the plists the device sends.
package mb2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"howett.net/plist"

	decplist "github.com/rbenzing/minutiae/internal/decode/plist"
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
	if err := checkBinary(b); err != nil {
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
func decodePlist(b []byte) (any, error) {
	var v any
	if _, err := safeUnmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// safeUnmarshal runs the decoder, converting a panic into an error.
func safeUnmarshal(b []byte, v any) (format int, err error) {
	defer func() {
		if r := recover(); r != nil {
			format, err = plist.InvalidFormat, fmt.Errorf("malformed plist (%v)", r)
		}
	}()
	return unmarshal(b, v)
}

// MaxPlistFile bounds a device-written plist file decoded by UnmarshalPlist.
const MaxPlistFile = 1 << 20

// UnmarshalPlist decodes a device-written plist file (such as Status.plist)
// into v. Input of more than MaxPlistFile bytes is refused. Anything the
// decoder would parse as binary (a "bplist" prefix) must first pass the same
// structural validation as a DeviceLink message; otherwise only an XML plist
// is accepted. A decoder panic is returned as an error. Every error carries the "mb2: " prefix.
func UnmarshalPlist(b []byte, v any) error {
	if len(b) > MaxPlistFile {
		return fmt.Errorf("mb2: plist of %d bytes exceeds limit", len(b))
	}
	switch {
	case bytes.HasPrefix(b, []byte("bplist")):
		if err := checkBinary(b); err != nil {
			return err
		}
	case decplist.LooksLikeXML(b):
		if err := decplist.PrescanXML(trimXMLLead(b), mb2Limits()); err != nil {
			return fmt.Errorf("mb2: %w", err)
		}
	default:
		return errors.New("mb2: not a binary or XML plist")
	}
	format, err := safeUnmarshal(b, v)
	if err != nil {
		return fmt.Errorf("mb2: %w", err)
	}
	if format != plist.BinaryFormat && format != plist.XMLFormat {
		return errors.New("mb2: not a binary or XML plist")
	}
	return nil
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

// mb2Limits are the bounds applied to every device-supplied plist: the numbers of the
// validator this package had before it used decode/plist.
func mb2Limits() decplist.Limits {
	return decplist.Limits{MaxNodes: 1 << 20, MaxDepth: 64, MaxPayload: maxMessage}
}

// checkBinary accepts only a well-formed binary plist within mb2Limits. XML, text and
// anything else is refused, so a DeviceLink frame is never XML.
func checkBinary(b []byte) error {
	if !bytes.HasPrefix(b, []byte("bplist")) {
		return errors.New("mb2: message is not a binary plist")
	}
	if err := decplist.Check(b, mb2Limits()); err != nil {
		return fmt.Errorf("mb2: %w", err)
	}
	return nil
}

// trimXMLLead drops an optional UTF-8 BOM and the ASCII whitespace after it. The prescan
// refuses whitespace before the XML declaration, but this package has always accepted it
// (the decoder does), so only the copy handed to the prescan is trimmed; the decoder still
// sees the original bytes and the prescan sees every other byte.
func trimXMLLead(b []byte) []byte {
	return bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n")
}
