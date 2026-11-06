// Package searchhint explains empty lexical searches for CLI and MCP callers.
package searchhint

import "strings"

// Hint explains an empty search.
type Hint struct {
	Code, Message string
}

// EmptyLexical reports an every-word miss only on the first page. A missing
// mode comes from older daemons that only supported lexical search.
func EmptyLexical(query, mode string, resultCount int, firstPage bool) *Hint {
	words := strings.Fields(query)
	if !firstPage || resultCount != 0 || (mode != "lexical" && mode != "") || len(words) < 2 {
		return nil
	}
	return &Hint{Code: "every_word_required", Message: "no issue contains every word; search again with fewer words"}
}
