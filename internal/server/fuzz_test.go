package server

import (
	"bytes"
	"testing"
)

func FuzzPOP3Top(f *testing.F) {
	f.Add([]byte("Subject: hello\r\n\r\nfirst\r\nsecond\r\n"), uint8(1))
	f.Add([]byte("malformed"), uint8(0))
	f.Fuzz(func(t *testing.T, raw []byte, lines uint8) {
		if len(raw) > 64<<10 {
			return
		}
		top := popTop(raw, int(lines))
		if !bytes.HasPrefix(raw, top) {
			t.Fatalf("TOP returned bytes outside the original message")
		}
	})
}

func FuzzEMLImport(f *testing.F) {
	f.Add([]byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: hello\r\n\r\nBody"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=X\r\n\r\n--X--\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		_, _ = importMessage(raw, "recipient@example.test")
	})
}
