package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// helpDebt records only the admin/documentation follow-up excluded from this PR.
// Completed entries must be removed; new commands must supply help.
//
//nolint:gosec // These are command paths, including token administration, not credentials.
var helpDebt = map[string]string{
	"kata agent-contract-hook":               "W13: admin and integration help follow-up",
	"kata agent-hook attention end":          "W13: admin and integration help follow-up",
	"kata agent-hook attention start":        "W13: admin and integration help follow-up",
	"kata agent-hook contract":               "W13: admin and integration help follow-up",
	"kata agent-hook install":                "W13: admin and integration help follow-up",
	"kata agent-hook instructions install":   "W13: admin and integration help follow-up",
	"kata agent-hook instructions status":    "W13: admin and integration help follow-up",
	"kata agent-hook instructions uninstall": "W13: admin and integration help follow-up",
	"kata agent-hook status":                 "W13: admin and integration help follow-up",
	"kata agent-hook uninstall":              "W13: admin and integration help follow-up",
	"kata audit closes":                      "W13: admin and integration help follow-up",
	"kata bridge bind":                       "W13: admin and integration help follow-up",
	"kata bridge pause":                      "W13: admin and integration help follow-up",
	"kata bridge reconcile":                  "W13: admin and integration help follow-up",
	"kata bridge resolve-comment":            "W13: admin and integration help follow-up",
	"kata bridge resolve-field":              "W13: admin and integration help follow-up",
	"kata bridge resume":                     "W13: admin and integration help follow-up",
	"kata bridge show":                       "W13: admin and integration help follow-up",
	"kata bridge unbind":                     "W13: admin and integration help follow-up",
	"kata connector field list":              "W13: admin and integration help follow-up",
	"kata connector field map":               "W13: admin and integration help follow-up",
	"kata connector field unmap":             "W13: admin and integration help follow-up",
	"kata connector list":                    "W13: admin and integration help follow-up",
	"kata connector status":                  "W13: admin and integration help follow-up",
	"kata daemon diagnose":                   "W13: admin and integration help follow-up",
	"kata daemon locate":                     "W13: admin and integration help follow-up",
	"kata daemon logs":                       "W13: admin and integration help follow-up",
	"kata daemon recover":                    "W13: admin and integration help follow-up",
	"kata daemon reload":                     "W13: admin and integration help follow-up",
	"kata daemon restart":                    "W13: admin and integration help follow-up",
	"kata daemon start":                      "W13: admin and integration help follow-up",
	"kata daemon status":                     "W13: admin and integration help follow-up",
	"kata daemon stop":                       "W13: admin and integration help follow-up",
	"kata digest":                            "W13: admin and integration help follow-up",
	"kata export":                            "W13: admin and integration help follow-up",
	"kata federation enable":                 "W13: admin and integration help follow-up",
	"kata federation enroll":                 "W13: admin and integration help follow-up",
	"kata federation enrollments list":       "W13: admin and integration help follow-up",
	"kata federation identity":               "W13: admin and integration help follow-up",
	"kata federation join":                   "W13: admin and integration help follow-up",
	"kata federation lease acquire":          "W13: admin and integration help follow-up",
	"kata federation lease force-release":    "W13: admin and integration help follow-up",
	"kata federation lease release":          "W13: admin and integration help follow-up",
	"kata federation lease renew":            "W13: admin and integration help follow-up",
	"kata federation lease steal":            "W13: admin and integration help follow-up",
	"kata federation leave":                  "W13: admin and integration help follow-up",
	"kata federation quarantine list":        "W13: admin and integration help follow-up",
	"kata federation quarantine retry":       "W13: admin and integration help follow-up",
	"kata federation quarantine show":        "W13: admin and integration help follow-up",
	"kata federation quarantine skip":        "W13: admin and integration help follow-up",
	"kata federation rebind":                 "W13: admin and integration help follow-up",
	"kata federation revoke":                 "W13: admin and integration help follow-up",
	"kata federation signing configure":      "W13: admin and integration help follow-up",
	"kata federation signing init-replay":    "W13: admin and integration help follow-up",
	"kata federation status":                 "W13: admin and integration help follow-up",
	"kata import":                            "W13: admin and integration help follow-up",
	"kata mcp serve":                         "W13: admin and integration help follow-up",
	"kata mcp status":                        "W13: admin and integration help follow-up",
	"kata openapi":                           "W13: admin and integration help follow-up",
	"kata projects create":                   "W13: admin and integration help follow-up",
	"kata projects detach":                   "W13: admin and integration help follow-up",
	"kata projects list":                     "W13: admin and integration help follow-up",
	"kata projects merge":                    "W13: admin and integration help follow-up",
	"kata projects purge":                    "W13: admin and integration help follow-up",
	"kata projects remove":                   "W13: admin and integration help follow-up",
	"kata projects rename":                   "W13: admin and integration help follow-up",
	"kata projects restore":                  "W13: admin and integration help follow-up",
	"kata projects rewrite-author":           "W13: admin and integration help follow-up",
	"kata projects show":                     "W13: admin and integration help follow-up",
	"kata storage postgres migrate":          "W13: admin and integration help follow-up",
	"kata storage postgres status":           "W13: admin and integration help follow-up",
	"kata sync notion disable":               "W13: admin and integration help follow-up",
	"kata sync notion enable":                "W13: admin and integration help follow-up",
	"kata sync notion once":                  "W13: admin and integration help follow-up",
	"kata sync notion status":                "W13: admin and integration help follow-up",
	"kata sync plane disable":                "W13: admin and integration help follow-up",
	"kata sync plane enable":                 "W13: admin and integration help follow-up",
	"kata sync plane once":                   "W13: admin and integration help follow-up",
	"kata sync plane status":                 "W13: admin and integration help follow-up",
	"kata sync twenty disable":               "W13: admin and integration help follow-up",
	"kata sync twenty enable":                "W13: admin and integration help follow-up",
	"kata sync twenty once":                  "W13: admin and integration help follow-up",
	"kata sync twenty status":                "W13: admin and integration help follow-up",
	"kata tokens create":                     "W13: admin and integration help follow-up",
	"kata tokens list":                       "W13: admin and integration help follow-up",
	"kata tokens revoke":                     "W13: admin and integration help follow-up",
	"kata tui":                               "W13: admin and integration help follow-up",
	"kata ui":                                "W13: admin and integration help follow-up",
	"kata update":                            "W13: admin and integration help follow-up",
}

func helpLintProblems(root *cobra.Command, debt map[string]string) []string {
	var problems []string
	seen := map[string]bool{}
	groups := map[string]bool{}
	for _, group := range root.Groups() {
		groups[group.ID] = true
	}
	walkVisibleHelp(root, func(cmd *cobra.Command) {
		path := cmd.CommandPath()
		seen[path] = true
		if cmd.Short == "" || utf8.RuneCountInString(cmd.Short) > 80 || strings.HasSuffix(cmd.Short, ".") {
			problems = append(problems, path+": Short must be nonempty, <=80 characters, without a final period")
		}
		if cmd.Parent() == root && !groups[cmd.GroupID] {
			problems = append(problems, path+": missing registered GroupID")
		}
		if !cmd.Runnable() || cmd.HasSubCommands() || cmd.Name() == "help" || cmd.Name() == "completion" {
			if _, exempt := debt[path]; exempt {
				problems = append(problems, path+": stale helpDebt entry (not a runnable leaf)")
			}
			return
		}
		lines, err := helpCommandLines(cmd.Example, false)
		missing := cmd.Long == "" || cmd.Example == "" || len(lines) == 0 || err != nil
		if reason, exempt := debt[path]; exempt {
			if reason == "" {
				problems = append(problems, path+": empty debt reason")
			}
			if !missing {
				problems = append(problems, path+": stale helpDebt entry")
			}
		} else if missing {
			problems = append(problems, path+": missing Long or Example")
		}
	})
	for path := range debt {
		if !seen[path] {
			problems = append(problems, path+": nonexistent or hidden helpDebt entry")
		}
	}
	return problems
}

func TestHelpLint(t *testing.T) {
	for _, problem := range helpLintProblems(newRootCmd(), helpDebt) {
		t.Error(problem)
	}
	// Inspect constructor descriptions before the catalog fills them in.
	saved := helpCatalog
	helpCatalog = nil
	root := newRootCmd()
	helpCatalog = saved
	for _, problem := range helpCatalogProblems(root, saved) {
		t.Error(problem)
	}
}

func helpCatalogProblems(root *cobra.Command, catalog map[string]helpDoc) []string {
	var problems []string
	seen := map[string]bool{}
	walkVisibleHelp(root, func(cmd *cobra.Command) {
		path := cmd.CommandPath()
		seen[path] = true
		if doc, exists := catalog[path]; exists {
			if doc.Long != "" && cmd.Long != "" {
				problems = append(problems, path+": Long has both constructor and catalog owners")
			}
			if doc.Example != "" && cmd.Example != "" {
				problems = append(problems, path+": Example has both constructor and catalog owners")
			}
		}
	})
	for path := range catalog {
		if !seen[path] {
			problems = append(problems, path+": catalog command does not exist")
		}
	}
	return problems
}

func TestHelpCatalogOwnership(t *testing.T) {
	root := &cobra.Command{Use: "kata", Long: "Existing description.", Example: "  kata --help"}
	for _, catalog := range []map[string]helpDoc{
		{"kata": {Long: "Second description."}},
		{"kata": {Example: "  kata version"}},
		{"kata missing": {Example: "  kata missing"}},
	} {
		if got := helpCatalogProblems(root, catalog); len(got) == 0 {
			t.Fatalf("accepted invalid catalog: %v", catalog)
		}
	}
}

func TestHelpCatalogPreservesConstructorExample(t *testing.T) {
	saved := helpCatalog
	helpCatalog = map[string]helpDoc{"kata": {Long: "Describe the command."}}
	t.Cleanup(func() { helpCatalog = saved })
	root := &cobra.Command{Use: "kata", Example: "  kata --help"}
	applyHelpCatalog(root)
	if root.Example != "  kata --help" {
		t.Fatalf("catalog erased constructor example: %q", root.Example)
	}
}

func TestHelpLintDebt(t *testing.T) {
	root := &cobra.Command{Use: "kata", Short: "root"}
	root.AddGroup(&cobra.Group{ID: "test", Title: "test"})
	leaf := &cobra.Command{Use: "leaf", Short: "leaf", GroupID: "test", Run: func(*cobra.Command, []string) {}}
	hidden := &cobra.Command{Use: "hidden", Hidden: true}
	hidden.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(leaf, hidden)
	if got := helpLintProblems(root, map[string]string{"kata leaf": "W13"}); len(got) != 0 {
		t.Fatal(got)
	}
	leaf.Long, leaf.Example = "Do the work.", "kata leaf"
	for _, debt := range []map[string]string{{"kata leaf": "W13"}, {"kata missing": "W13"}, {"kata hidden child": "W13"}} {
		if got := helpLintProblems(root, debt); len(got) == 0 {
			t.Fatalf("accepted invalid debt: %v", debt)
		}
	}
	leaf.AddCommand(&cobra.Command{Use: "child", Short: "child", Long: "Do child work.", Example: "kata leaf child", Run: func(*cobra.Command, []string) {}})
	if got := helpLintProblems(root, map[string]string{"kata leaf": "W13"}); len(got) == 0 {
		t.Fatal("accepted stale leaf debt after command gained a child")
	}
}

func TestHelpLintShortCharacters(t *testing.T) {
	root := &cobra.Command{Use: "kata", Short: strings.Repeat("é", 80)}
	if got := helpLintProblems(root, nil); len(got) != 0 {
		t.Fatalf("80-character Short rejected: %v", got)
	}
	root.Short += "é"
	if got := helpLintProblems(root, nil); len(got) == 0 {
		t.Fatal("81-character Short accepted")
	}
}
