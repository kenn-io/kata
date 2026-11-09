package mcpserver

import (
	"context"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/agentguide"
)

type skillGuide struct {
	name, title, summary string
	// tools lists the catalog tools this guide teaches, for task-to-tool search.
	tools []string
}

var skillGuides = []skillGuide{
	{name: "setup", title: "Set up MCP-only instructions", summary: "Get exact managed text and a safe edit plan for your own local file editor."},
	{name: "quickstart", title: "Kata MCP quickstart", summary: "Follow typed tool examples from search through evidence-backed completion."},
	{name: "workflow", title: "Kata agent workflow", summary: "Search first, reuse or create one issue, claim it and close with evidence.", tools: []string{"kata.search", "kata.create", "kata.show", "kata.comment", "kata.edit", "kata.set_label", "kata.next", "kata.ready", "kata.claim", "kata.unassign", "kata.status", "kata.set_metadata", "kata.set_schedule", "kata.set_deadline", "kata.close", "kata.reopen", "kata.audit_closes", "kata.delete", "kata.purge", "kata.inbox", "kata.wait", "kata.events"}},
	{name: "issues", title: "Find, create and update issues", summary: "Reuse existing work, create with idempotency and connect related issues.", tools: []string{"kata.search", "kata.create", "kata.show", "kata.comment", "kata.set_label", "kata.edit"}},
	{name: "claiming", title: "Pick up and own work", summary: "Find available work, claim it and release ownership safely.", tools: []string{"kata.next", "kata.ready", "kata.claim", "kata.unassign", "kata.status", "kata.show"}},
	{name: "closing", title: "Close with evidence", summary: "Complete work with typed evidence or hand back unfinished work.", tools: []string{"kata.set_label", "kata.close", "kata.audit_closes", "kata.reopen"}},
	{name: "teammates", title: "Teammates, inbox and attention", summary: "Attribute contributions, coordinate requests and signal attention with work.* metadata.", tools: []string{"kata.comment", "kata.create", "kata.claim", "kata.inbox", "kata.set_metadata", "kata.wait"}},
	{name: "scheduling", title: "Park, schedule and set deadlines", summary: "Defer work, set deadlines and return deferred issues to the queue.", tools: []string{"kata.set_schedule", "kata.set_deadline", "kata.set_metadata"}},
	{name: "events", title: "Follow activity with events", summary: "Resume activity from a cursor and recover from resets.", tools: []string{"kata.events"}},
}

// SkillGuides returns guide names in catalog order.
func SkillGuides() []string {
	names := make([]string, len(skillGuides))
	for i, guide := range skillGuides {
		names[i] = guide.name
	}
	return names
}

// SkillMarkdown returns the rendered Markdown of a named guide.
func SkillMarkdown(name string) (string, bool) {
	return agentguide.MCPGuide(name)
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
	Skills       []SkillSummary `json:"skills"`
	ManagedBlock string         `json:"managed_block,omitempty" jsonschema:"Exact MCP-only managed text returned by setup; the caller must install it with its own file editor"`
}

func registerSkillsTool(server *sdkmcp.Server) {
	addTool(server, "kata.skills", "Kata how-to guides", `Native guides for this server's Kata tools. Use name=setup for exact MCP-only managed instructions and a caller-side edit plan (no files are read/written); quickstart for typed examples; workflow before your first write. Other topics: issues, claiming, closing, teammates, scheduling, events. Omit name for the catalog. Fixed content only; no arbitrary files or paths.`, toolHints(true, false, false), toolHandlers{}.skills)
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
	if name == "setup" {
		output.ManagedBlock, _ = agentguide.ManagedBlock("mcp")
	}
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: text.String()}}}, output, nil
}
