package planesync

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

// descriptionMarkdown converts local content only; it never retrieves links,
// images, stylesheets or attachments. Validate URLs before the converter emits
// Markdown links, which otherwise could retain active unsafe URL schemes.
func descriptionMarkdown(input, pageURL string) (string, error) {
	if len(input) > maxDescriptionBytes || !utf8.ValidString(input) || strings.ContainsRune(input, '\x00') {
		return "", fmt.Errorf("plane description must be valid UTF-8 without NUL and at most 1 MiB")
	}
	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return "", fmt.Errorf("cannot parse Plane description HTML")
	}
	page, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("invalid Plane description base URL")
	}
	type entry struct {
		node  *html.Node
		depth int
	}
	stack := []entry{{doc, 0}}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if e.depth > 256 {
			return "", fmt.Errorf("plane description HTML nesting exceeds 256 levels")
		}
		attrs := e.node.Attr[:0]
		for _, attr := range e.node.Attr {
			if attr.Key != "href" && attr.Key != "src" {
				attrs = append(attrs, attr)
				continue
			}
			value := strings.TrimSpace(attr.Val)
			link, err := url.Parse(value)
			if err != nil || strings.IndexFunc(value, unicode.IsControl) >= 0 || link.User != nil {
				continue
			}
			switch strings.ToLower(link.Scheme) {
			case "", "https", "http", "mailto":
			default:
				continue
			}
			attr.Val = page.ResolveReference(link).String()
			attrs = append(attrs, attr)
		}
		e.node.Attr = attrs
		for child := e.node.FirstChild; child != nil; child = child.NextSibling {
			stack = append(stack, entry{child, e.depth + 1})
		}
	}
	conv := converter.NewConverter(converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(commonmark.WithLinkEmptyHrefBehavior(commonmark.LinkBehaviorSkip)), strikethrough.NewStrikethroughPlugin(), table.NewTablePlugin()))
	markdown, err := conv.ConvertNode(doc)
	if err != nil {
		return "", fmt.Errorf("cannot convert Plane description to Markdown")
	}
	if len(markdown) > maxDescriptionBytes || !utf8.Valid(markdown) {
		return "", fmt.Errorf("plane description Markdown exceeds 1 MiB")
	}
	return strings.TrimSpace(string(markdown)), nil
}
