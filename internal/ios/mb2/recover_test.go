package mb2

import (
	"bytes"
	"strings"
	"testing"
)

func TestRecvConvertsDecoderPanicToError(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec(&buf)
	if err := c.Send([]any{"DLMessageDisconnect"}); err != nil {
		t.Fatal(err)
	}
	orig := unmarshal
	t.Cleanup(func() { unmarshal = orig })
	unmarshal = func([]byte, any) (int, error) { panic("boom") }
	msg, err := c.Recv()
	if err == nil || !strings.Contains(err.Error(), "malformed plist") {
		t.Fatalf("msg=%v err=%v", msg, err)
	}
}
