package mailmime

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	message "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	HTMLBodyLimit    = 512 << 10
	inlineImageLimit = 1 << 20
	inlineBudget     = 2 << 20
)

type HTML struct {
	HasHTML bool
	HasText bool
	TooLong bool
	body    []byte
	images  map[string]string
}

func InspectHTML(raw []byte, forDisplay bool) HTML {
	var view HTML
	reader, _ := message.CreateReader(bytes.NewReader(raw))
	if reader == nil {
		return view
	}
	defer reader.Close()
	imageBytes := 0
	for {
		part, _ := reader.NextPart()
		if part == nil {
			break
		}
		var header message.AttachmentHeader
		var inline bool
		switch h := part.Header.(type) {
		case *message.InlineHeader:
			header, inline = message.AttachmentHeader{Header: h.Header}, true
		case *message.AttachmentHeader:
			header = *h
		default:
			continue
		}
		media, _, _ := header.ContentType()
		filename, _ := header.Filename()
		if inline && filename == "" {
			switch media {
			case "text/plain":
				text, _ := io.ReadAll(io.LimitReader(part.Body, 256<<10))
				view.HasText = view.HasText || len(bytes.TrimSpace(text)) != 0
			case "text/html":
				view.HasHTML = true
				if forDisplay && len(view.body) == 0 && !view.TooLong {
					body, err := io.ReadAll(io.LimitReader(part.Body, HTMLBodyLimit+1))
					if len(body) > HTMLBodyLimit {
						view.TooLong = true
					} else if err == nil {
						view.body = body
					}
				}
			}
		}
		if !forDisplay && view.HasHTML && view.HasText {
			break
		}
		if !forDisplay || imageBytes >= inlineBudget || !inlineImageType(media) {
			continue
		}
		id := strings.Trim(strings.TrimSpace(header.Get("Content-ID")), "<>")
		if id == "" || len(id) > 512 || view.images[id] != "" {
			continue
		}
		picture, err := io.ReadAll(io.LimitReader(part.Body, inlineImageLimit+1))
		if err != nil || len(picture) > inlineImageLimit || len(picture) > inlineBudget-imageBytes ||
			http.DetectContentType(picture) != media {
			continue
		}
		dimensions, _, err := image.DecodeConfig(bytes.NewReader(picture))
		if err != nil || dimensions.Width <= 0 || dimensions.Height <= 0 || dimensions.Width > 4096 || dimensions.Height > 4096 {
			continue
		}
		if view.images == nil {
			view.images = make(map[string]string)
		}
		view.images[id] = "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(picture)
		imageBytes += len(picture)
	}
	return view
}

func inlineImageType(media string) bool {
	switch media {
	case "image/png", "image/jpeg", "image/gif":
		return true
	}
	return false
}

var emailHTMLPolicy = func() *bluemonday.Policy {
	policy := bluemonday.NewPolicy()
	policy.AllowElements("a", "abbr", "address", "article", "b", "blockquote", "br", "caption", "center",
		"code", "col", "colgroup", "dd", "div", "dl", "dt", "em", "font", "h1", "h2", "h3", "h4",
		"h5", "h6", "hr", "i", "img", "li", "ol", "p", "pre", "s", "section", "small", "span",
		"strong", "sub", "sup", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul")
	policy.SkipElementsContent("button", "iframe", "math", "noscript", "object", "script", "select", "style", "svg", "template", "textarea", "title")
	policy.RequireParseableURLs(true)
	policy.AllowURLSchemes("http", "https", "mailto", "cid")
	policy.AllowAttrs("href").Matching(regexp.MustCompile(`(?i)^(https?://|mailto:)`)).OnElements("a")
	policy.AllowAttrs("src").Matching(regexp.MustCompile(`(?i)^cid:[^\s]{1,512}$`)).OnElements("img")
	policy.AllowAttrs("title", "lang", "dir").Globally()
	policy.AllowAttrs("alt", "width", "height").OnElements("img")
	policy.AllowAttrs("colspan", "rowspan").OnElements("td", "th")
	policy.AllowAttrs("width", "height", "align", "valign", "bgcolor").OnElements("table", "tr", "td", "th")
	policy.AllowAttrs("color", "face", "size").OnElements("font")
	policy.AllowStyles("color", "background-color", "font-family", "font-size", "font-style", "font-weight",
		"text-align", "text-decoration", "vertical-align", "line-height", "border", "border-color",
		"border-collapse", "border-radius", "padding", "padding-top", "padding-right", "padding-bottom",
		"padding-left", "margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
		"width", "min-width", "max-width", "height").Globally()
	return policy
}()

// SanitizeHTML applies the shared mail allowlist to a UTF-8 fragment.
func SanitizeHTML(body []byte) []byte {
	return emailHTMLPolicy.SanitizeBytes(body)
}

func (view HTML) RenderFragment() ([]byte, error) {
	if view.TooLong {
		return []byte("<p>HTML 正文超过 512 KiB，可下载原始邮件查看。</p>"), nil
	}
	if len(view.body) == 0 {
		return []byte("<p>无法显示 HTML 正文，可下载原始邮件查看。</p>"), nil
	}
	fragment := emailHTMLPolicy.SanitizeBytes(bytes.ToValidUTF8(view.body, []byte("\uFFFD")))
	nodes, err := html.ParseFragment(bytes.NewReader(fragment), &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"})
	if err != nil {
		return nil, err
	}
	remaining := 3 * inlineBudget / 2
	var output bytes.Buffer
	for _, node := range nodes {
		rewriteMailNodes(node, view.images, &remaining)
		if err := html.Render(&output, node); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), nil
}

func rewriteMailNodes(node *html.Node, images map[string]string, remaining *int) {
	if node.Type == html.ElementNode {
		switch node.DataAtom {
		case atom.Img:
			for i := range node.Attr {
				attr := &node.Attr[i]
				if attr.Key != "src" {
					continue
				}
				prefix, value, ok := strings.Cut(attr.Val, ":")
				id, err := url.PathUnescape(value)
				source := images[id]
				if !ok || !strings.EqualFold(prefix, "cid") || err != nil || source == "" || len(source) > *remaining {
					node.Attr = append(node.Attr[:i], node.Attr[i+1:]...)
				} else {
					*remaining -= len(source)
					attr.Val = source
				}
				break
			}
		case atom.A:
			for i := range node.Attr {
				if node.Attr[i].Key != "href" {
					continue
				}
				link, err := url.Parse(node.Attr[i].Val)
				if err != nil || (!strings.EqualFold(link.Scheme, "mailto") && (link.Hostname() == "" || link.User != nil)) {
					node.Attr = append(node.Attr[:i], node.Attr[i+1:]...)
				} else {
					node.Attr = append(node.Attr, html.Attribute{Key: "target", Val: "_blank"},
						html.Attribute{Key: "rel", Val: "noopener noreferrer"})
				}
				break
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		rewriteMailNodes(child, images, remaining)
	}
}
