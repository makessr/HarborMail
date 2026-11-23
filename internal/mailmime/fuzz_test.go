package mailmime

import "testing"

func FuzzInspectHTML(f *testing.F) {
	f.Add([]byte("Content-Type: text/html; charset=utf-8\r\n\r\n<p>Hello</p>"))
	f.Add([]byte("Content-Type: multipart/alternative; boundary=mail\r\n\r\n--mail--\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		view := InspectHTML(raw, true)
		if view.HasHTML {
			_, _ = view.RenderFragment()
		}
	})
}
