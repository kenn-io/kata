// Package docs embeds Kata's user-facing Markdown documentation so the MCP
// server can answer documentation questions without network access.
package docs

import "embed"

// FS holds the overview, getting-started, guide, workflow, reference, and
// operations pages. Design notes and contributor pages stay out.
//
//go:embed index.md get-started/*.md guide/*.md workflows/*.md reference/*.md operations/*.md
var FS embed.FS
