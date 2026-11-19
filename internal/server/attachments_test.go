package server

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestAttachmentDownloadKeepsOriginalTextEncoding(t *testing.T) {
	raw := strings.Join([]string{
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=mail",
		"",
		"--mail",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"Please see the attached file.",
		"--mail",
		"Content-Type: text/plain; charset=iso-8859-1",
		"Content-Disposition: attachment; filename*=UTF-8''caf%C3%A9.txt",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"caf=E9",
		"--mail--",
		"",
	}, "\r\n")

	count := 0
	visitAttachments([]byte(raw), func(info attachmentInfo, body io.Reader) bool {
		count++
		if info.Part != 1 || info.Name != "café.txt" {
			t.Errorf("unexpected attachment metadata: %+v", info)
		}
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, []byte("caf\xe9")) {
			t.Errorf("attachment bytes = %x, error = %v", got, err)
		}
		return true
	})
	if count != 1 {
		t.Fatalf("found %d attachments, want 1", count)
	}
}
