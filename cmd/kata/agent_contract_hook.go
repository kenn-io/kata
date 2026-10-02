package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// newAgentContractHookCmd emits Codex's SessionStart envelope without requiring
// a stdin payload. The canonical contract remains in agent_contract.go.
func newAgentContractHookCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "agent-contract-hook",
		Short: "Emit the agent contract as Codex SessionStart context",
		Long: "Emit the built-in agent contract as a Codex SessionStart response.\n\n" +
			"--source selects a local UTF-8 prompt file that replaces the entire contract.\n" +
			"Relative paths stay within the current directory; parent traversal and symlink escapes fail.\n" +
			"Absolute paths use the specified file; a missing file uses the built-in contract.\n" +
			"An empty file supplies an intentionally empty prompt. Stdin is ignored.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			text, err := readAgentContractSource(source, cmd.Flags().Changed("source"))
			if err != nil {
				return err
			}
			response := struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}{}
			response.HookSpecificOutput.HookEventName = "SessionStart"
			response.HookSpecificOutput.AdditionalContext = text
			return json.MarshalWrite(cmd.OutOrStdout(), response)
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "local UTF-8 prompt file (missing file uses built-in contract)")
	return cmd
}

func readAgentContractSource(path string, specified bool) (string, error) {
	if !specified {
		return agentContractText, nil
	}
	if path == "" {
		return "", agentHookUsage("--source requires a nonempty local file path")
	}
	var (
		file *os.File
		err  error
	)
	if filepath.IsAbs(path) {
		var info os.FileInfo
		info, err = os.Stat(path) //nolint:gosec // G304: absolute paths are explicit local files selected with --source.
		if err == nil && !info.Mode().IsRegular() {
			return "", fmt.Errorf("prompt source %q must be a regular UTF-8 text file", path)
		}
		if err == nil {
			file, err = os.Open(path) //nolint:gosec // G304: absolute paths are explicit local files selected with --source.
		}
	} else {
		root, rootErr := os.OpenRoot(".")
		if rootErr != nil {
			return "", fmt.Errorf("open prompt source %q: %w", path, rootErr)
		}
		defer func() { _ = root.Close() }()
		var info os.FileInfo
		info, err = root.Stat(path)
		if err == nil && !info.Mode().IsRegular() {
			return "", fmt.Errorf("prompt source %q must be a regular UTF-8 text file", path)
		}
		if err == nil {
			file, err = root.Open(path)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return agentContractText, nil
	}
	if err != nil {
		return "", fmt.Errorf("open prompt source %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect prompt source %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("prompt source %q must be a regular UTF-8 text file", path)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read prompt source %q: %w", path, err)
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("prompt source %q is not valid UTF-8", path)
	}
	return string(content), nil
}
