package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/agentguide"
)

type skillGuide struct {
	name, title, summary string
	fragments            []string
	commands             []string
}

var skillGuides = []skillGuide{
	{name: "workflow", title: "Kata agent workflow", summary: "Search first, claim work, track attention and close with evidence."},
	{name: "issues", title: "Find, create and update issues", summary: "Reuse existing work, create with idempotency and connect related issues.", fragments: []string{"intro", "real-work", "search-create-update", "relationships"}, commands: []string{"kata search", "kata create", "kata show", "kata comment", "kata label add", "kata edit"}},
	{name: "claiming", title: "Pick up and own work", summary: "Find available work, claim it and release ownership safely.", fragments: []string{"claiming"}, commands: []string{"kata next", "kata ready", "kata claim", "kata unassign", "kata status", "kata show"}},
	{name: "closing", title: "Close with evidence", summary: "Complete work with typed evidence or hand back unfinished work.", fragments: []string{"closing", "destructive"}, commands: []string{"kata label add", "kata close", "kata audit closes", "kata reopen"}},
	{name: "teammates", title: "Teammates, inbox and attention", summary: "Attribute contributions and coordinate requests with teammates.", fragments: []string{"teammates"}, commands: []string{"kata comment", "kata create", "kata claim", "kata notify", "kata inbox"}},
	{name: "scheduling", title: "Park, schedule and set deadlines", summary: "Defer work, set deadlines and return deferred issues to the queue.", fragments: []string{"scheduling"}, commands: []string{"kata schedule", "kata deadline", "kata meta set", "kata meta unset"}},
	{name: "events", title: "Follow activity with events", summary: "Resume activity from a cursor and recover from resets.", fragments: []string{"events"}, commands: []string{"kata events"}},
}

var cliOnlyFragments = []string{"output-flags", "mutation-comments", "daemon-recovery", "remote-daemon"}

// SkillGuides returns guide names in catalog order.
func SkillGuides() []string {
	names := make([]string, len(skillGuides))
	for i, guide := range skillGuides {
		names[i] = guide.name
	}
	return names
}

// SkillSource returns the verbatim CLI text served by a named guide.
func SkillSource(name string) (string, bool) {
	for _, guide := range skillGuides {
		if guide.name != name {
			continue
		}
		if name == "workflow" {
			return agentguide.Contract, true
		}
		var source strings.Builder
		for _, id := range guide.fragments {
			for _, fragment := range agentguide.Fragments {
				if fragment.ID == id {
					source.WriteString(fragment.Text)
				}
			}
		}
		return source.String(), true
	}
	return "", false
}

const skillMCPDifferences = `## MCP differences

- The actor and project scope are fixed when the server starts. Ignore ` + "`--project`, `--workspace`" + ` and author-variable guidance.
- In multi-project scope, use ` + "`project#ref`" + ` for every existing-issue read and write. Supply ` + "`project`" + ` only to tools whose schema has it, such as ` + "`kata.create`, `kata.search` and `kata.inbox`" + `.
- ` + "`--agent` and `--json`" + ` do not apply. Tools return structured results.
- Load a section with its ` + "`kata.load_*`" + ` tool, then refresh the tool list.
- ` + "`kata.create` and `kata.comment`" + ` require ` + "`idempotency_key`" + `.
- CLI ` + "`--comment`" + ` on a mutation becomes a separate ` + "`kata.comment`" + ` call.
- Teammate attribution: calls use the server's startup default teammate. Pass ` + "`teammate`" + ` on ` + "`kata.comment` and `kata.create`" + ` when sibling agents share one server; an empty value suppresses the default.
`

var skillMarkdown = sync.OnceValue(func() map[string]string {
	guides := make(map[string]string, len(skillGuides))
	for _, guide := range skillGuides {
		var b strings.Builder
		b.WriteString("# " + guide.title + "\n\nThis guide is the text the Kata CLI ships. Commands map to MCP tools in the\ntable at the end.\n\n")
		source, _ := SkillSource(guide.name)
		b.WriteString(source)
		b.WriteString("\n")
		if guide.name == "workflow" {
			b.WriteString(skillMCPDifferences + "\n")
		}
		b.WriteString(renderSkillCommands(guide.name))
		guides[guide.name] = b.String()
	}
	return guides
})

// SkillMarkdown returns the rendered Markdown of a named guide.
func SkillMarkdown(name string) (string, bool) {
	md, ok := skillMarkdown()[name]
	return md, ok
}

// SkillsInput selects a fixed guide, or lists the catalog when Name is blank.
type SkillsInput struct {
	Name string `json:"name,omitempty"`
}

// SkillSummary describes a guide without repeating its Markdown body.
type SkillSummary struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// SkillsOutput identifies the guides returned as text content.
type SkillsOutput struct {
	Skills []SkillSummary `json:"skills"`
}

func registerSkillsTool(server *sdkmcp.Server) {
	addTool(server, "kata.skills", "Kata how-to guides", `How-to guides for this server's own Kata tools: the agent workflow contract (search first, claim, attention, close with evidence), issues, claiming, closing, teammates and inbox, scheduling, events. A short fixed catalog served by this server. It is not a general skill reader and cannot read files, paths or other skills. Call with no name to list the guides. Call kata.skills {"name":"workflow"} before your first write.`, toolHints(true, false, false), toolHandlers{}.skills)
}

func (toolHandlers) skills(_ context.Context, _ *sdkmcp.CallToolRequest, input SkillsInput) (*sdkmcp.CallToolResult, SkillsOutput, error) {
	name := strings.TrimSpace(input.Name)
	output := SkillsOutput{Skills: make([]SkillSummary, 0, len(skillGuides))}
	var text strings.Builder
	for _, guide := range skillGuides {
		if name != "" && name != guide.name {
			continue
		}
		output.Skills = append(output.Skills, SkillSummary{Name: guide.name, Title: guide.title, Summary: guide.summary})
		if name == "" {
			fmt.Fprintf(&text, "- %s: %s. %s\n", guide.name, guide.title, guide.summary)
		} else {
			md, _ := SkillMarkdown(name)
			text.WriteString(md)
		}
	}
	if len(output.Skills) == 0 {
		return nil, SkillsOutput{}, fmt.Errorf("unknown guide %q; available: %s", name, strings.Join(SkillGuides(), ", "))
	}
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text.String()}}}, output, nil
}
