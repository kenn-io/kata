package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

type outputMode string

const (
	outputHuman    outputMode = "human"
	outputJSON     outputMode = "json"
	outputAgent    outputMode = "agent"
	outputContract outputMode = "contract"

	agentFormatVersion = 1
)

type agentIssueMutation struct {
	Issue struct {
		ShortID      string  `json:"short_id"`
		QualifiedID  string  `json:"qualified_id"`
		Title        string  `json:"title"`
		Status       string  `json:"status"`
		ClosedReason *string `json:"closed_reason"`
		Owner        *string `json:"owner"`
		DeletedAt    *string `json:"deleted_at"`
	} `json:"issue"`
	Event *struct {
		Payload string `json:"payload"`
	} `json:"event"`
	Label struct {
		Label string `json:"label"`
	} `json:"label"`
	Changed       bool    `json:"changed"`
	Reused        bool    `json:"reused,omitempty"`
	PreviousOwner *string `json:"previous_owner,omitempty"`
}

// outputFormatFlag records every --format occurrence in order. pflag calls Set
// once per occurrence; the last value is what String() reports for help text.
type outputFormatFlag struct {
	values *[]string
}

func (f outputFormatFlag) Set(s string) error {
	*f.values = append(*f.values, s)
	return nil
}

func (f outputFormatFlag) String() string {
	if f.values == nil || len(*f.values) == 0 {
		return ""
	}
	return (*f.values)[len(*f.values)-1]
}

func (f outputFormatFlag) Type() string { return "string" }

func printAgentMutation(cmd *cobra.Command, verb string, bs []byte, extra func(io.Writer, agentIssueMutation) error) error {
	var m agentIssueMutation
	if err := json.Unmarshal(bs, &m); err != nil {
		return err
	}
	return printAgentMutationDecoded(cmd.OutOrStdout(), verb, m, false, extra)
}

func printAgentMutationDecoded(
	w io.Writer,
	verb string,
	m agentIssueMutation,
	includeChangedTrue bool,
	extra func(io.Writer, agentIssueMutation) error,
) error {
	if !flags.Quiet {
		if _, err := fmt.Fprintf(w, "OK %s %s", verb, m.Issue.ShortID); err != nil {
			return err
		}
		if m.Reused {
			if _, err := fmt.Fprint(w, " reused=true"); err != nil {
				return err
			}
		}
		if !m.Changed || includeChangedTrue {
			if _, err := fmt.Fprintf(w, " changed=%t", m.Changed); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if m.Issue.ShortID != "" && m.Issue.Title != "" {
		if _, err := fmt.Fprintf(w, "Issue: %s %s\n", m.Issue.ShortID, agentValue(m.Issue.Title)); err != nil {
			return err
		}
	}
	if m.Issue.Status != "" {
		if err := writeAgentField(w, "Status", agentValue(m.Issue.Status)); err != nil {
			return err
		}
	}
	if extra != nil {
		return extra(w, m)
	}
	return nil
}

// outputSelection is the output-mode flags exactly as the user typed them. It
// is written only by flag parsing (and by preParse on the error path) and
// never by resolution, so it can never be contradicted by its own result.
type outputSelection struct {
	formats []string // every --format occurrence, in order
	json    bool     // --json as typed
	agent   bool     // --agent as typed
}

// resolve reduces the selection to one output mode. On failure it returns the
// error together with a best-effort fallback mode to render that error in:
// the agreed-upon human, JSON, or agent mode, else human. Contract participates
// in validation but is not an error-rendering mode.
//
// importLegacy marks `kata import`, whose --format also carries the legacy
// kata|beads source-format overload; those values select no output mode and
// are skipped rather than rejected. contractAllowed marks commands such as
// quickstart whose output contract is a supported format.
func (s outputSelection) resolve(importLegacy, contractAllowed bool) (outputMode, error) {
	wanted := "human, json, or agent"
	if contractAllowed {
		wanted = "human, json, agent, or contract"
	}
	selected := make([]outputMode, 0, len(s.formats)+2)
	fallbackSelected := make([]outputMode, 0, len(s.formats)+2)
	badFormat := ""
	for _, format := range s.formats {
		format = outputFormatValue(format, importLegacy)
		if format == "" {
			continue
		}
		switch mode := outputMode(format); mode {
		case outputHuman, outputJSON, outputAgent:
			selected = append(selected, mode)
			fallbackSelected = append(fallbackSelected, mode)
		case outputContract:
			if contractAllowed {
				selected = append(selected, outputContract)
				continue
			}
			fallthrough
		default:
			if badFormat == "" {
				badFormat = format
			}
		}
	}
	if s.json {
		selected = append(selected, outputJSON)
		fallbackSelected = append(fallbackSelected, outputJSON)
	}
	if s.agent {
		selected = append(selected, outputAgent)
		fallbackSelected = append(fallbackSelected, outputAgent)
	}

	agreed := outputHuman
	consistent := true
	if len(selected) > 0 {
		agreed = selected[0]
		for _, mode := range selected[1:] {
			if mode != agreed {
				consistent = false
				break
			}
		}
	}
	fallback := outputHuman
	if len(fallbackSelected) > 0 {
		fallback = fallbackSelected[0]
		for _, mode := range fallbackSelected[1:] {
			if mode != fallback {
				fallback = outputHuman
				break
			}
		}
	}

	// An unsupported format outranks a conflict, matching the order the
	// selection is read in: a value that names no mode at all is reported
	// before modes that merely disagree.
	if badFormat != "" {
		return fallback, &cliError{
			Message:  "unsupported output format " + strconv.Quote(badFormat) + " (want " + wanted + ")",
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	if !consistent {
		return fallback, &cliError{Message: "conflicting output modes", Kind: kindUsage, ExitCode: ExitUsage}
	}
	return agreed, nil
}

// currentOutputMode returns the resolved output mode. PersistentPreRunE
// resolves it exactly once before any RunE runs; an empty Mode means
// resolution has not happened (the error path, where cobra failed first), and
// human is the safe answer there.
func currentOutputMode() outputMode {
	if flags.Mode != "" {
		return flags.Mode
	}
	return outputHuman
}

func resolveOutputModeForCommand(cmd *cobra.Command) (outputMode, error) {
	return flags.Sel.resolve(isImportCommand(cmd), supportsContractOutput(cmd))
}

// preParse walks argv once, returning the command those args select and the
// output flags they carry. It is used only on the error path, where cobra
// failed before PersistentPreRunE could resolve a mode, so it is total: it
// never returns an error and never panics, and an argv it cannot interpret
// simply yields the root command and an empty selection.
//
// A `-`-prefixed token is always a flag, never a command name — that is the
// point where the two walkers this replaces used to disagree, one of them
// abandoning command resolution at a shorthand like -q. An unrecognized
// *positional* stops command resolution (the command path cannot continue past
// it) but flag scanning continues to the end of argv. Everything after `--` is
// positional by definition and is not scanned.
//
// --format, --json, and --agent are root-persistent, so they are valid on every
// command and are recorded wherever they appear.
func preParse(root *cobra.Command, args []string) (*cobra.Command, outputSelection) {
	var sel outputSelection
	if root == nil {
		return nil, sel
	}
	cmd := root
	commandPathStopped := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			if !commandPathStopped {
				if next := childCommandByName(cmd, arg); next != nil {
					cmd = next
					continue
				}
				commandPathStopped = true
			}
			continue
		}
		name, value, hasValue, ok := splitLongFlag(arg)
		if !ok {
			// A shorthand cluster (-q, -1). None of the output flags has a
			// shorthand, so there is nothing to record; a shorthand's value
			// token, if any, is handled by the positional branch above.
			continue
		}
		switch name {
		case "json":
			if hasValue {
				if parsed, err := strconv.ParseBool(value); err == nil {
					sel.json = parsed
				}
			} else {
				sel.json = true
			}
		case "agent":
			if hasValue {
				if parsed, err := strconv.ParseBool(value); err == nil {
					sel.agent = parsed
				}
			} else {
				sel.agent = true
			}
		case "format":
			if hasValue {
				sel.formats = append(sel.formats, value)
			} else if i+1 < len(args) {
				sel.formats = append(sel.formats, args[i+1])
				i++
			}
		default:
			if flagConsumesValue(cmd, name) && !hasValue && i+1 < len(args) {
				i++
			}
		}
	}
	return cmd, sel
}

// flagConsumesValue reports whether cmd's long flag `name` takes a following
// argument, so preParse can skip that argument instead of mistaking it for a
// command name (`--workspace /path list`).
func flagConsumesValue(cmd *cobra.Command, name string) bool {
	if cmd == nil || name == "" {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		flag = cmd.PersistentFlags().Lookup(name)
	}
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup(name)
	}
	return flag != nil && flag.Value.Type() != "bool"
}

func splitLongFlag(arg string) (name, value string, hasValue bool, ok bool) {
	if !strings.HasPrefix(arg, "--") || arg == "--" {
		return "", "", false, false
	}
	trimmed := strings.TrimPrefix(arg, "--")
	if trimmed == "" {
		return "", "", false, false
	}
	if before, after, found := strings.Cut(trimmed, "="); found {
		return before, after, true, true
	}
	return trimmed, "", false, true
}

func outputFormatValue(format string, importLegacy bool) string {
	if importLegacy && isImportLegacySourceFormat(format) {
		return ""
	}
	return format
}

func isImportCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "import"
}

func supportsContractOutput(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "quickstart"
}

func isImportLegacySourceFormat(format string) bool {
	switch strings.TrimSpace(format) {
	case "kata", "beads":
		return true
	default:
		return false
	}
}

func emitAgentError(w io.Writer, command string, err error) {
	var cli *cliError
	if !errors.As(err, &cli) {
		cli = &cliError{
			Message:  err.Error(),
			Kind:     kindInternal,
			ExitCode: ExitInternal,
		}
	}
	if command == "" {
		command = "kata"
	}
	_, _ = fmt.Fprintf(w, "ERR %s %s: %s\n", command, cli.Kind, firstLine(cli.Message)) //nolint:gosec // G705: CLI stderr error text, not HTML.
}

func agentValue(s string) string {
	clean := textsafe.Block(s)
	if clean == "" {
		return `""`
	}
	if strings.IndexFunc(clean, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '\\' || unicode.IsControl(r)
	}) >= 0 {
		return strconv.Quote(clean)
	}
	return clean
}

func writeAgentField(w io.Writer, name, value string) error {
	_, err := fmt.Fprintf(w, "%s: %s\n", name, value)
	return err
}

func agentFencedText(s string) string {
	clean := textsafe.Block(s)
	fence := "```"
	for strings.Contains(clean, fence) {
		fence += "`"
	}
	return fence + "text\n" + clean + "\n" + fence + "\n"
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
