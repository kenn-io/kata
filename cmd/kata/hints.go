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
	body = bytes.TrimSpace(body)
	encoded, err := json.Marshal([]cliHint{hint})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(body[:len(body)-1])
	out.WriteString(`,"hints":`)
	out.Write(encoded)
	out.WriteByte('}')
	return out.Bytes(), nil
}

// retryFlags are the daemon, scope, and output flags every retry keeps. Actor
// flags such as --as and --teammate stay out of a suggested command.
var retryFlags = []string{"daemon", "project", "workspace", "format", "json", "agent", "quiet"}

// retryCommand returns a shell command that runs kata command with set, then
// each flag from retryFlags and keep that the caller changed, then args after
// "--". It omits every other flag.
func retryCommand(cmd *cobra.Command, command string, set, keep []string, args ...string) string {
	parts := append([]string{"kata", command}, set...)
	for _, name := range slices.Concat(retryFlags, keep) {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}
		values := []string{f.Value.String()}
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			values = slice.GetSlice()
		}
		for _, value := range values {
			switch f.Value.Type() {
			case "bool":
				if value == "true" {
					parts = append(parts, "--"+name)
				} else {
					parts = append(parts, "--"+name+"=false")
				}
				continue
			case "stringSlice":
				// pflag reads each occurrence as CSV, so a value holding a
				// comma or quote must be encoded to arrive whole.
				var field strings.Builder
				w := csv.NewWriter(&field)
				_ = w.Write([]string{value}) // writes to memory, which cannot fail
				w.Flush()
				value = strings.TrimSuffix(field.String(), "\n")
			}
			parts = append(parts, "--"+name, shellQuoteArg(value))
		}
	}
	if len(args) > 0 {
		parts = append(parts, "--")
		for _, arg := range args {
			parts = append(parts, shellQuoteArg(arg))
		}
	}
	return strings.Join(parts, " ")
}
