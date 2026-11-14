package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/kit/agenthook"
)

type cliHint struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
}

// reportHint adds hint to a JSON response body, or prints it to stderr unless
// --quiet, and returns the body to render.
func reportHint(cmd *cobra.Command, body []byte, hint cliHint) ([]byte, error) {
	if currentOutputMode() != outputJSON {
		if flags.Quiet {
			return body, nil
		}
		_, err := fmt.Fprintln(cmd.ErrOrStderr(), "Hint: "+hint.Message)
		return body, err
	}
	encoded, err := json.Marshal([]cliHint{hint})
	body = bytes.TrimSpace(body)
	return slices.Concat(body[:len(body)-1], []byte(`,"hints":`), encoded, []byte("}")), err
}

// retryCommand returns a shell command that runs kata command with set, then
// each daemon, scope, output, and keep flag the caller changed.
// Actor flags such as --as and --teammate stay out of it.
func retryCommand(cmd *cobra.Command, command string, set, keep []string) (agenthook.Commands, error) {
	parts := append([]string{command}, set...)
	for _, name := range slices.Concat([]string{"daemon", "project", "workspace", "format", "json", "agent", "quiet"}, keep) {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}
		values := []string{f.Value.String()}
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			values = slice.GetSlice()
		}
		for _, value := range values {
			if f.Value.Type() == "stringSlice" {
				if value == "" {
					parts = append(parts, "--"+name, `""`)
					continue
				}
				var field strings.Builder
				w := csv.NewWriter(&field)
				_ = w.Write([]string{value}) // strings.Builder writes cannot fail.
				w.Flush()
				value = strings.TrimSuffix(field.String(), "\n")
			}
			if f.Value.Type() != "bool" {
				parts = append(parts, "--"+name, value)
			} else if value == "true" {
				parts = append(parts, "--"+name)
			}
		}
	}
	if args := cmd.Flags().Args(); len(args) > 0 {
		parts = append(parts, "--")
		parts = append(parts, args...)
	}
	return agenthook.BuildCommand("kata", parts...)
}
