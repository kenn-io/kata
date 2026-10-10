package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func init() {
	cobra.AddTemplateFunc("kataGlobalFlags", kataGlobalFlags)
}

// applyRootHelp keeps the discoverable CLI guide and grouping in one place.
func applyRootHelp(root *cobra.Command) {
	root.Short = "issue tracker for coding agents"
	root.Long = kataRootHelp
	for _, group := range []cobra.Group{
		{ID: "find", Title: "Find and read work:"},
		{ID: "change", Title: "Change issues:"},
		{ID: "coord", Title: "Coordinate agents:"},
		{ID: "setup", Title: "Set up and diagnose:"},
		{ID: "admin", Title: "Administer (rarely needed by agents):"},
	} {
		root.AddGroup(&group)
	}
	for _, child := range root.Commands() {
		child.GroupID = kataCommandGroups[child.Name()]
	}
	root.SetHelpCommandGroupID("setup")
	root.SetCompletionCommandGroupID("setup")
	root.SetUsageTemplate(kataUsageTemplate)
}

var kataCommandGroups = map[string]string{
	"search":              "find",
	"list":                "find",
	"ready":               "find",
	"next":                "find",
	"show":                "find",
	"status":              "find",
	"inbox":               "find",
	"labels":              "find",
	"whoami":              "find",
	"create":              "change",
	"edit":                "change",
	"comment":             "change",
	"claim":               "change",
	"assign":              "change",
	"unassign":            "change",
	"label":               "change",
	"meta":                "change",
	"schedule":            "change",
	"deadline":            "change",
	"notify":              "change",
	"move":                "change",
	"close":               "change",
	"reopen":              "change",
	"wait":                "coord",
	"events":              "coord",
	"digest":              "coord",
	"audit":               "coord",
	"quickstart":          "coord",
	"agent-hook":          "coord",
	"agent-contract-hook": "coord",
	"cron":                "admin",
	"init":                "setup",
	"doctor":              "setup",
	"health":              "setup",
	"daemon":              "setup",
	"mcp":                 "setup",
	"ui":                  "setup",
	"tui":                 "setup",
	"update":              "setup",
	"version":             "setup",
	"projects":            "admin",
	"tokens":              "admin",
	"storage":             "admin",
	"federation":          "admin",
	"sync":                "admin",
	"bridge":              "admin",
	"connector":           "admin",
	"export":              "admin",
	"import":              "admin",
	"openapi":             "admin",
	"delete":              "admin",
	"restore":             "admin",
	"purge":               "admin",
}

func kataGlobalFlags(cmd *cobra.Command) string {
	var names []string
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		name := "--" + flag.Name
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			name = "-" + flag.Shorthand + "/" + name
		}
		if flag.Value.Type() != "bool" {
			name += " <" + flag.Value.Type() + ">"
		}
		names = append(names, name)
	})
	return strings.Join(names, ", ")
}

const kataRootHelp = `Kata is an issue tracker for coding agents and the people who direct them.
Commands talk to a kata daemon (local and auto-started by default;
KATA_SERVER selects a remote one) and act on the project bound to the
current directory.

Agent workflow (add --agent to each command):
  kata search "<terms>" --agent      # 1. reuse an existing issue first
  kata create "<title>" --body "<context>" --idempotency-key <stable-key> --agent
  kata claim <ref> --agent           # 2. take ownership
  kata meta set <ref> work.attention ok --agent      # ok | needs-human | stuck
  kata meta set <ref> work.attention_msg "<one-line state>" --agent
  kata close <ref> --done -m "<what changed and how it was verified>" --commit <sha> --agent
  kata label add <ref> needs-review --comment "<what remains>" --agent   # not done
Full agent contract: kata quickstart --format contract

Refs: abc4 (short id in the bound project), project#abc4 (any project), or the
26-character ULID. Numeric refs are rejected.
Project: found from .kata.toml at or above --workspace (default: cwd). Create the
binding with kata init, or pass --project <name>.
Output: human text by default. --agent prints one OK/ERR line plus key=value
rows; --json prints the full JSON envelope for scripts. Errors go to stderr.
Exit codes: 0 ok, 1 internal, 2 usage, 3 validation, 4 not found, 5 conflict,
6 confirmation required, 7 daemon unavailable, 8 wait timeout.
Environment: KATA_AUTHOR actor; KATA_TEAMMATE teammate attribution;
KATA_INBOX_USER inbox address (actor or actor/teammate); KATA_SERVER remote
daemon URL; KATA_HOME local daemon state; KATA_AUTH_TOKEN remote auth.`

// Cobra v1.10.2 default usage template, with compact inherited flags.
const kataUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global: {{kataGlobalFlags .}}. Details: kata --help{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`
