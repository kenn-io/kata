// Package searchhint explains empty lexical searches for CLI and MCP callers.
package searchhint

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Hint describes an empty search and a shorter query to try.
type Hint struct {
	Code, Message, Term string
}

// EmptyLexical reports an every-word miss only on the first page. A missing
// mode comes from older daemons that only supported lexical search.
func EmptyLexical(query, mode string, resultCount int, firstPage bool) *Hint {
	words := strings.Fields(query)
	if !firstPage || resultCount != 0 || (mode != "lexical" && mode != "") || len(words) < 2 {
		return nil
	}
	longest := slices.MaxFunc(words, func(x, y string) int { return cmp.Compare(utf8.RuneCountInString(x), utf8.RuneCountInString(y)) })
	return &Hint{Code: "every_word_required", Message: "no issue contains every word; search again with fewer words", Term: longest}
}
