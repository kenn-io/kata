package mcpserver

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

func TestSearchAndReadBundledDocs(t *testing.T) {
	session := connectTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test-version",
	})

	found := callAdministrationTool(t, session, "kata.search_docs", map[string]any{"query": "progressive tool catalog", "limit": 3})
	matches := found["matches"].([]any)
	require.NotEmpty(t, matches)
	first := matches[0].(map[string]any)
	require.Equal(t, "reference/mcp.md#progressive-tool-catalog", first["id"])
	require.Equal(t, "Progressive tool catalog", first["heading"])
	require.Equal(t, "https://katatracker.com/docs/reference/mcp/#progressive-tool-catalog", first["url"])
	require.NotContains(t, first["excerpt"], "\n")

	read := callAdministrationTool(t, session, "kata.read_doc", map[string]any{"id": first["id"]})
	content := read["content"].(string)
	require.True(t, strings.HasPrefix(content, "## Progressive tool catalog"), content)
	require.Contains(t, content, "kata.load_docs")
	require.NotContains(t, content, "## Scheduling and someday", "a section ends at the next heading")

	page := callAdministrationTool(t, session, "kata.read_doc", map[string]any{"id": "reference/mcp.md"})
	require.Equal(t, "Model Context Protocol server", page["heading"])
	require.Equal(t, "https://katatracker.com/docs/reference/mcp/", page["url"])

	callToolError(t, session, "kata.read_doc", map[string]any{"id": "design/architecture.md"})
	callToolError(t, session, "kata.search_docs", map[string]any{"query": "?!"})
	none := callAdministrationTool(t, session, "kata.search_docs", map[string]any{"query": "zzzz-no-such-word"})
	require.Empty(t, none["matches"])
}

func TestBundledDocSectionsHaveUniqueIDsAndStaySmall(t *testing.T) {
	sections, err := docSections()
	require.NoError(t, err)
	require.NotEmpty(t, sections)
	seen := map[string]bool{}
	for _, section := range sections {
		require.False(t, seen[section.id], "duplicate section id %s", section.id)
		seen[section.id] = true
		require.LessOrEqual(t, len(section.content), 32<<10, "%s is too large to return whole", section.id)
	}
	require.True(t, seen["get-started/quickstart.md"])
	require.True(t, seen["index.md"])
}

func TestDocSectionsSkipFencedHeadingsAndNumberRepeats(t *testing.T) {
	sections, err := loadDocSections(fstest.MapFS{"guide/example.md": {Data: []byte(
		"---\ntitle: Example page\n---\n\n# Example page\n\nIntro.\n\n## Setup\n\n```sh\n# not a heading\n```\n\n## Setup\n\nAgain.\n",
	)}})
	require.NoError(t, err)
	ids := make([]string, 0, len(sections))
	for _, section := range sections {
		ids = append(ids, section.id)
		require.Equal(t, "Example page", section.document)
	}
	require.Equal(t, []string{"guide/example.md", "guide/example.md#setup", "guide/example.md#setup_1"}, ids)
	require.Contains(t, sections[1].content, "# not a heading")
	require.Equal(t, "https://katatracker.com/docs/guide/example/#setup_1", sections[2].url)

	untitled, err := loadDocSections(fstest.MapFS{"workflows/plain.md": {Data: []byte("# Plain page\n\n## Usage\n\nBody.\n")}})
	require.NoError(t, err)
	require.Len(t, untitled, 2)
	require.Equal(t, "Plain page", untitled[1].document, "a page without frontmatter takes its title from the leading heading")
}

func TestDocSectionsRespectClosingFences(t *testing.T) {
	for _, tt := range []struct {
		name  string
		block string
	}{
		{"nested backticks", "````text\n```text\n# not a heading\n```\n````"},
		{"tagged backticks", "```text\n```text\n# not a heading\n```"},
		{"nested tildes", "~~~~text\n~~~\n# not a heading\n~~~~"},
		{"different character and longer closer", "```text\n~~~\n# not a heading\n````` \t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sections := splitDocSections("guide/example.md", "# Example\n\n## Sample\n\n"+tt.block+"\n\n## After\n\nText.\n")
			require.Len(t, sections, 3)
			require.Equal(t, "guide/example.md#sample", sections[1].id)
			require.Contains(t, sections[1].content, "# not a heading")
			require.Equal(t, "guide/example.md#after", sections[2].id)
			require.NotContains(t, sections[1].content, "## After")
		})
	}
}
