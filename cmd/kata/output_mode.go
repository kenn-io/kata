package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type outputMode string

const (
	outputHuman outputMode = "human"
	outputJSON  outputMode = "json"
	outputAgent outputMode = "agent"
)

const agentFormatVersion = 1

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

func resolveOutputModeArgs(args []string, format string, jsonFlag, agentFlag bool) (outputMode, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return resolveOutputModeValues(format, jsonFlag, agentFlag)
		case arg == "--json":
			jsonFlag = true
		case arg == "--agent":
			agentFlag = true
		case arg == "--format" && i+1 < len(args):
			format = args[i+1]
			i++
		case strings.HasPrefix(arg, "--format="):
			format = strings.TrimPrefix(arg, "--format=")
		}
	}
	return resolveOutputModeValues(format, jsonFlag, agentFlag)
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
	_, _ = fmt.Fprintf(w, "ERR %s %s: %s\n", command, cli.Kind, cli.Message)
}
