package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
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
