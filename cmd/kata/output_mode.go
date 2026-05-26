package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/kata/internal/textsafe"
)

type outputMode string

const (
	outputHuman outputMode = "human"
	outputJSON  outputMode = "json"
	outputAgent outputMode = "agent"

	agentFormatVersion = 1
)

func resolveOutputModeValues(format string, jsonFlag, agentFlag bool) (outputMode, error) {
	var selected []outputMode
	if format != "" {
		switch outputMode(format) {
		case outputHuman, outputJSON, outputAgent:
			selected = append(selected, outputMode(format))
		default:
			return "", &cliError{
				Message:  "unsupported output format " + strconv.Quote(format) + " (want human, json, or agent)",
				Kind:     kindUsage,
				ExitCode: ExitUsage,
			}
		}
	}
	if jsonFlag {
		selected = append(selected, outputJSON)
	}
	if agentFlag {
		selected = append(selected, outputAgent)
	}
	if len(selected) == 0 {
		return outputHuman, nil
	}
	first := selected[0]
	for _, mode := range selected[1:] {
		if mode != first {
			return "", &cliError{Message: "conflicting output modes", Kind: kindUsage, ExitCode: ExitUsage}
		}
	}
	return first, nil
}

func currentOutputMode() outputMode {
	if flags.Mode != "" {
		return flags.Mode
	}
	if flags.Agent {
		return outputAgent
	}
	if flags.JSON {
		return outputJSON
	}
	return outputHuman
}

func resolveOutputModeForCommand(cmd *cobra.Command) (outputMode, error) {
	format := flags.Format
	if isImportCommand(cmd) && isImportLegacySourceFormat(format) {
		format = ""
	}
	return resolveOutputModeValues(format, flags.JSON, flags.Agent)
}

func resolveOutputModeArgs(args []string, format string, jsonFlag, agentFlag bool) (outputMode, error) {
	return resolveOutputModeArgsForCommand(args, format, jsonFlag, agentFlag, false, nil)
}

func resolveOutputModeArgsForCommand(
	args []string,
	format string,
	jsonFlag, agentFlag bool,
	importLegacy bool,
	root *cobra.Command,
) (outputMode, error) {
	cmd := root
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return resolveOutputModeValues(outputFormatValue(format, importLegacy), jsonFlag, agentFlag)
		}
		name, value, hasValue, ok := splitLongFlag(arg)
		if !ok {
			if next := childCommandByName(cmd, arg); next != nil {
				cmd = next
			}
			continue
		}
		switch name {
		case "json":
			if !rawFlagKnown(cmd, name) {
				continue
			}
			if hasValue {
				if parsed, err := strconv.ParseBool(value); err == nil {
					jsonFlag = parsed
				}
			} else {
				jsonFlag = true
			}
		case "agent":
			if !rawFlagKnown(cmd, name) {
				continue
			}
			if hasValue {
				if parsed, err := strconv.ParseBool(value); err == nil {
					agentFlag = parsed
				}
			} else {
				agentFlag = true
			}
		case "format":
			if !rawFlagKnown(cmd, name) {
				continue
			}
			if hasValue {
				format = value
			} else if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		default:
			if rawFlagConsumesValue(cmd, name) && !hasValue && i+1 < len(args) {
				i++
			}
		}
	}
	return resolveOutputModeValues(outputFormatValue(format, importLegacy), jsonFlag, agentFlag)
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

func rawFlagKnown(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return isRootPersistentFlag(name)
	}
	return lookupRawFlag(cmd, name) != nil
}

func rawFlagConsumesValue(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return isRootStringFlag(name)
	}
	flag := lookupRawFlag(cmd, name)
	return flag != nil && flag.Value.Type() != "bool"
}

func lookupRawFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if cmd == nil {
		return nil
	}
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	if flag := cmd.PersistentFlags().Lookup(name); flag != nil {
		return flag
	}
	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		return flag
	}
	if root := cmd.Root(); root != nil {
		return root.PersistentFlags().Lookup(name)
	}
	return nil
}

func isRootPersistentFlag(name string) bool {
	switch name {
	case "format", "json", "agent", "quiet", "as", "workspace", "project":
		return true
	default:
		return false
	}
}

func isRootStringFlag(name string) bool {
	switch name {
	case "format", "as", "workspace", "project":
		return true
	default:
		return false
	}
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
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + "text\n" + textsafe.Block(s) + "\n" + fence + "\n"
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
