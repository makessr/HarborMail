package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestHTMLMailAllowsLocalImagesAndDropsActiveContent(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	content := `<h1>Newsletter</h1><table><tr><td>Summary</td></tr></table>
		<img src="cid:logo%40example.test" alt="logo">
		<img src="https://tracker.example.test/pixel" alt="remote">
		<img src="cid:svg@example.test" alt="svg">
		<a href="https://example.test/read">Read</a><a href="/private">Relative</a>
		<script>active-content-marker</script><form action="https://example.test/form"><input name="text"></form>`
	raw := strings.Join([]string{
		"MIME-Version: 1.0",
		"Content-Type: multipart/related; boundary=mail",
		"",
		"--mail",
		"Content-Type: text/html; charset=utf-8",
		"",
		content,
		"--mail",
		"Content-Type: image/png",
		"Content-ID: <logo@example.test>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(picture.Bytes()),
		"--mail",
		"Content-Type: image/svg+xml",
		"Content-ID: <svg@example.test>",
		"",
		"<svg></svg>",
		"--mail--",
		"",
	}, "\r\n")
	view := inspectHTMLMail([]byte(raw), true)
	if !view.hasHTML || view.hasText {
		t.Fatalf("unexpected text alternatives: HTML=%t, plain=%t", view.hasHTML, view.hasText)
	}
	fragment, err := view.renderFragment()
	if err != nil {
		t.Fatal(err)
	}
	got := string(fragment)
	for _, keep := range []string{"<h1>Newsletter</h1>", "<td>Summary</td>", `src="data:image/png;base64,`, `rel="noopener noreferrer"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("missing %q in %s", keep, got)
		}
	}
	for _, reject := range []string{"tracker.example.test", "image/svg", "active-content-marker", "<form", "/private"} {
		if strings.Contains(got, reject) {
			t.Errorf("unexpected %q in %s", reject, got)
		}
	}
}
