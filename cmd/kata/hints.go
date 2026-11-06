package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type cliHint struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next"`
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
	separator := []byte(`,"hints":`)
	if len(bytes.TrimSpace(body[1:len(body)-1])) == 0 {
		separator = []byte(`"hints":`)
	}
	return slices.Concat(body[:len(body)-1], separator, encoded, []byte("}")), err
}

// retryCommand returns a shell command that runs kata command with set, then
// each daemon, scope, output, and keep flag the caller changed, then term
// after "--". Actor flags such as --as and --teammate stay out of it.
func retryCommand(cmd *cobra.Command, command string, set, keep []string, term string) string {
	return retryCommandForOS(cmd, command, set, keep, term, runtime.GOOS)
}

func retryCommandForOS(cmd *cobra.Command, command string, set, keep []string, term, goos string) string {
	quote := shellQuoteArg
	if goos == "windows" {
		quote = func(s string) string {
			if isShellSafeArg(s) && !strings.ContainsAny(s, "@%,") {
				return s
			}
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		}
	}
	parts := append([]string{"kata", command}, set...)
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
				var field strings.Builder
				w := csv.NewWriter(&field)
				_ = w.Write([]string{value}) // strings.Builder writes cannot fail.
				w.Flush()
				value = strings.TrimSuffix(field.String(), "\n")
			}
			if f.Value.Type() != "bool" {
				parts = append(parts, "--"+name, quote(value))
			} else if value == "true" {
				parts = append(parts, "--"+name)
			}
		}
	}
	if term != "" {
		parts = append(parts, "--", quote(term))
	}
	return strings.Join(parts, " ")
}
