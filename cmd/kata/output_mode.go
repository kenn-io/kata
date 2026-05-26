package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type outputMode string

const (
	outputHuman outputMode = "human"
	outputJSON  outputMode = "json"
	outputAgent outputMode = "agent"
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

func resolveOutputModeForCommand(cmd *cobra.Command) (outputMode, error) {
	format := flags.Format
	if isImportCommand(cmd) && isImportLegacySourceFormat(format) {
		format = ""
	}
	return resolveOutputModeValues(format, flags.JSON, flags.Agent)
}

func resolveOutputModeArgs(args []string, format string, jsonFlag, agentFlag bool) (outputMode, error) {
	return resolveOutputModeArgsForCommand(args, format, jsonFlag, agentFlag, false)
}

func resolveOutputModeArgsForCommand(
	args []string,
	format string,
	jsonFlag, agentFlag bool,
	importLegacy bool,
) (outputMode, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return resolveOutputModeValues(outputFormatValue(format, importLegacy), jsonFlag, agentFlag)
		}
		name, value, hasValue, ok := splitLongFlag(arg)
		if !ok {
			continue
		}
		switch name {
		case "json":
			if !hasValue {
				jsonFlag = true
			}
		case "agent":
			if !hasValue {
				agentFlag = true
			}
		case "format":
			if hasValue {
				format = value
			} else if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "as", "workspace", "project":
			if !hasValue && i+1 < len(args) {
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

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
