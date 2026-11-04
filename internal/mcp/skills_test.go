package mcpserver

import (
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	katadocs "go.kenn.io/kata/docs"
	"go.kenn.io/kata/internal/agentguide"
	"go.kenn.io/kata/internal/storageadmin"
)

func TestSkillFragmentsArePartitioned(t *testing.T) {
	assigned := map[string]int{}
	for _, guide := range skillGuides {
		for _, id := range guide.fragments {
			assigned[id]++
		}
	}
	for _, id := range cliOnlyFragments {
		assigned[id]++
	}
	for _, fragment := range agentguide.Fragments {
		require.Equal(t, 1, assigned[fragment.ID], fragment.ID)
		delete(assigned, fragment.ID)
	}
	require.Empty(t, assigned, "unknown fragment IDs")
}

func mutatedNameGen(names []string) *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		name := rapid.SampledFrom(names).Draw(t, "guide")
		return rapid.SampledFrom([]string{
			strings.ToUpper(name), " " + name + "\t", "\u00a0" + name + "\u2003",
			name[:1] + " " + name[1:], "../" + name, "/" + name, name + ".md", "x" + name, name + "x",
		}).Draw(t, "mutation")
	})
}

func TestSkillsLookupOutcomes(t *testing.T) {
	// Rapid configures case counts per process; keep this test's existing budget.
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	names := []string{"workflow", "issues", "claiming", "closing", "teammates", "scheduling", "events", "cron"}
	gen := rapid.OneOf(rapid.String(), rapid.SampledFrom(names), mutatedNameGen(names), rapid.SampledFrom([]string{"", "\t\n\u2003", "/tmp/guide.md", "..", "workflow\x00"}))
	rapid.Check(t, func(rt *rapid.T) {
		raw := gen.Draw(rt, "name")
		res, out, err := (toolHandlers{}).skills(t.Context(), nil, SkillsInput{Name: raw})
		name := strings.TrimSpace(raw)
		if name != "" && !slices.Contains(names, name) {
			require.EqualError(rt, err, fmt.Sprintf("unknown guide %q; available: %s", name, strings.Join(names, ", ")))
			require.Nil(rt, res)
			require.Empty(rt, out.Skills)
			return
		}
		require.NoError(rt, err)
		require.Len(rt, res.Content, 1)
		text := res.Content[0].(*sdkmcp.TextContent).Text
		if name == "" {
			var got []string
			for _, summary := range out.Skills {
				got = append(got, summary.Name)
				require.Contains(rt, text, "- "+summary.Name+": "+summary.Title+". "+summary.Summary)
			}
			require.Equal(rt, names, got)
		} else {
			require.Len(rt, out.Skills, 1)
			require.Equal(rt, name, out.Skills[0].Name)
			md, _ := SkillMarkdown(name)
			require.Equal(rt, md, text)
		}
	})
}

func TestSkillsExportedLookupsUseExactNames(t *testing.T) {
	// Keep the property-test budget consistent with the former Hegel check.
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	names := []string{"workflow", "issues", "claiming", "closing", "teammates", "scheduling", "events", "cron"}
	gen := rapid.OneOf(rapid.String(), rapid.SampledFrom(names), mutatedNameGen(names))
	for _, tc := range []struct {
		name   string
		lookup func(string) (string, bool)
	}{
		{"source", SkillSource},
		{"markdown", SkillMarkdown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				name := gen.Draw(rt, "name")
				body, ok := tc.lookup(name)
				require.Equal(rt, slices.Contains(names, name), ok)
				if !ok {
					require.Empty(rt, body)
				}
			})
		})
	}
}

func TestSkillsSchemaBoundary(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version"})
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"64 ASCII runes", map[string]any{"name": strings.Repeat("a", 64)}, "unknown guide"},
		{"65 ASCII runes", map[string]any{"name": strings.Repeat("a", 65)}, `validating "arguments"`},
		{"padded valid name", map[string]any{"name": "  workflow \t"}, ""},
		{"absent", map[string]any{}, ""},
		{"whitespace", map[string]any{"name": "\u2003\t"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.skills", Arguments: tc.args})
			require.NoError(t, err)
			require.Equal(t, tc.want != "", result.IsError, "%s", mustJSON(t, result))
			if tc.want != "" {
				require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, tc.want)
			}
		})
	}
}

func TestSkillsGetReturnsOneTextBlock(t *testing.T) {
	session := connectTestServer(t)
	for _, name := range SkillGuides() {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.skills", Arguments: map[string]any{"name": name}})
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Len(t, result.Content, 1)
		md, _ := SkillMarkdown(name)
		require.Equal(t, md, result.Content[0].(*sdkmcp.TextContent).Text)
		var out map[string]any
		require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &out))
		require.Len(t, out, 1)
		entries := out["skills"].([]any)
		require.Len(t, entries, 1)
		entry := entries[0].(map[string]any)
		require.Len(t, entry, 3)
		require.Equal(t, name, entry["name"])
		require.NotEmpty(t, entry["title"])
		require.NotEmpty(t, entry["summary"])
	}
}

func TestSkillsDeterministicAndScopeIndependent(t *testing.T) {
	admin, err := storageadmin.New(storageadmin.Config{Root: t.TempDir(), SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, Name: "spoke-project", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, {ID: 2, Name: "other-project", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAW"}})
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	for scopeIndex, scope := range []*Scope{bound, allowlist, NewAllScope()} {
		for _, tokenAdmin := range []bool{false, true} {
			for _, teammate := range []string{"", "teammate-1"} {
				for _, storageAdmin := range []bool{false, true} {
					t.Run(fmt.Sprintf("%d/%t/%s/%t", scopeIndex, tokenAdmin, teammate, storageAdmin), func(t *testing.T) {
						options := Options{Client: inertTestClient(t), Actor: "example-agent", Version: "test-version"}
						options.Scope = scope
						options.EnableTokenAdmin = tokenAdmin
						options.Teammate = teammate
						if storageAdmin {
							options.StorageAdmin = admin
						}
						session := connectRawTestServerWithOptions(t, options)
						defer func() { require.NoError(t, session.Close()) }()
						names := append([]string{""}, SkillGuides()...)
						for _, name := range names {
							want, output, err := (toolHandlers{}).skills(t.Context(), nil, SkillsInput{Name: name})
							require.NoError(t, err)
							got, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.skills", Arguments: map[string]any{"name": name}})
							require.NoError(t, err)
							require.False(t, got.IsError)
							require.Equal(t, want.Content, got.Content)
							require.JSONEq(t, string(mustJSON(t, output)), string(mustJSON(t, got.StructuredContent)))
						}
					})
				}
			}
		}
	}
}

func TestSkillGuidesIncludeTheirFragmentsVerbatim(t *testing.T) {
	for _, guide := range skillGuides {
		md, ok := SkillMarkdown(guide.name)
		require.True(t, ok)
		var source strings.Builder
		for _, id := range guide.fragments {
			found := false
			for _, fragment := range agentguide.Fragments {
				if fragment.ID == id {
					require.Contains(t, md, fragment.Text)
					source.WriteString(fragment.Text)
					found = true
				}
			}
			require.True(t, found, id)
		}
		if guide.name == "cron" {
			page, err := katadocs.Page("guide/cron-authoring.md")
			require.NoError(t, err)
			require.Contains(t, md, page)
			source.WriteString(page)
		}
		if guide.name == "workflow" {
			require.Contains(t, md, agentguide.Contract)
			require.Contains(t, md, "## MCP differences")
			source.WriteString(agentguide.Contract)
		}
		got, ok := SkillSource(guide.name)
		require.True(t, ok)
		require.Equal(t, source.String(), got)
	}
	for _, name := range []string{"missing", "Workflow", "workflow.md", "../workflow"} {
		md, ok := SkillMarkdown(name)
		require.False(t, ok)
		require.Empty(t, md)
		source, ok := SkillSource(name)
		require.False(t, ok)
		require.Empty(t, source)
	}
}

func TestSkillGuidesStayBounded(t *testing.T) {
	for _, name := range SkillGuides() {
		md, ok := SkillMarkdown(name)
		require.True(t, ok)
		require.LessOrEqual(t, len(md), 16384, name)
	}
}

func TestSkillCatalogOrder(t *testing.T) {
	want := []string{"workflow", "issues", "claiming", "closing", "teammates", "scheduling", "events", "cron"}
	require.Equal(t, want, SkillGuides())
	returned := SkillGuides()
	returned[0] = "changed"
	require.Equal(t, want, SkillGuides(), "callers must not mutate the catalog")
}

func TestSkillCommandToolsAreRegistered(t *testing.T) {
	session := connectTestServer(t)
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := toolNames(listed.Tools)
	for _, row := range SkillCommands() {
		for _, tool := range row.Tools {
			require.Contains(t, names, tool, row.CLI)
			if tool == "kata.skills" {
				continue // Core guides are always registered and have no section loader.
			}
			loader, ok := sectionLoaderForTool(tool)
			require.True(t, ok, tool)
			require.Contains(t, names, loader)
		}
	}
}

func TestSkillGuideCommandsExist(t *testing.T) {
	rows := SkillCommands()
	var all []string
	for _, row := range rows {
		all = append(all, row.CLI)
	}
	for _, name := range SkillGuides() {
		for _, key := range SkillGuideCommands(name) {
			require.Contains(t, all, key, name)
		}
	}
	require.Equal(t, all, SkillGuideCommands("workflow"))
	require.Empty(t, SkillGuideCommands("missing"))
	rows[0].Tools[0] = "changed"
	require.Equal(t, "kata.search", SkillCommands()[0].Tools[0])
}

func TestSkillTableEscapesCells(t *testing.T) {
	row := renderCommandRow(SkillCommand{CLI: "kata x", Tools: []string{"kata.x"}, Note: "a|b"}, "kata.load_x")
	require.Equal(t, "| `kata x` | `kata.x` | `kata.load_x` | a\\|b |", row)
	for _, c := range SkillCommands() {
		require.NotContains(t, c.CLI+c.Note+strings.Join(c.Tools, ""), "\n", c.CLI)
		require.NotContains(t, c.CLI+c.Note+strings.Join(c.Tools, ""), "\r", c.CLI)
	}
	require.Equal(t, "| `kata notify` | none | — | CLI only |", renderCommandRow(SkillCommand{CLI: "kata notify", Note: "CLI only"}, ""))
}

func TestSkillGuideCommandCopiesAreIndependent(t *testing.T) {
	for _, name := range SkillGuides() {
		want := slices.Clone(SkillGuideCommands(name))
		returned := SkillGuideCommands(name)
		require.NotEmpty(t, returned, name)
		returned[0] = "changed"
		require.Equal(t, want, SkillGuideCommands(name), name)
	}
}

func TestSkillGuidesRenderTheirRows(t *testing.T) {
	for _, name := range SkillGuides() {
		md, _ := SkillMarkdown(name)
		for _, key := range SkillGuideCommands(name) {
			for _, row := range SkillCommands() {
				if row.CLI == key {
					loader := ""
					if len(row.Tools) != 0 {
						loader, _ = sectionLoaderForTool(row.Tools[0])
					}
					require.Contains(t, md, renderCommandRow(row, loader), name)
				}
			}
		}
	}
}

func TestSkillTableCoreToolHasNoLoader(t *testing.T) {
	row := renderCommandRow(SkillCommand{CLI: "kata cron guide", Tools: []string{"kata.skills"}}, "")
	require.Equal(t, "| `kata cron guide` | `kata.skills` | — |  |", row)
}
