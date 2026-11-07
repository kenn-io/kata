// Package docs embeds Kata's user-facing Markdown documentation so the CLI
// and MCP server can serve version-matched guides without network access.
package docs

import (
	"embed"
	"strings"
)

// FS holds the overview, getting-started, guide, workflow, reference, and
// operations pages. Design notes and contributor pages stay out.
//
//go:embed index.md get-started/*.md guide/*.md workflows/*.md reference/*.md operations/*.md
var FS embed.FS

// Page returns an embedded page as Markdown, normalizing CRLF and removing
// one leading, complete frontmatter block. The body is otherwise unchanged.
// Pages printed outside the site should use absolute documentation links.
func Page(name string) (string, error) {
	raw, err := FS.ReadFile(name)
	if err != nil {
		return "", err
	}
	return pageBody(string(raw)), nil
}

func pageBody(raw string) string {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		for rest != "" {
			line, body, more := strings.Cut(rest, "\n")
			if line == "---" {
				return body
			}
			if !more {
				break
			}
			rest = body
		}
	}
	return text
}
