package agentguide

import (
	"fmt"
	"strings"
)

// Managed markers are shared by both modes so switching replaces one owned span.
const (
	BlockBegin = "<!-- BEGIN KATA (managed by `kata init --with-agents`) -->"
	BlockEnd   = "<!-- END KATA -->"
)

// ManagedBlock returns exact managed text, without a final separator newline.
// An empty mode preserves the CLI default used by existing callers and hooks.
func ManagedBlock(mode string) (string, error) {
	text := Contract
	switch mode {
	case "", "cli":
	case "mcp":
		text = MCPContract
	default:
		return "", fmt.Errorf("unknown agent instruction mode %q", mode)
	}
	return BlockBegin + "\n" + text + BlockEnd, nil
}

// UpsertManagedBlock updates only one unambiguous Kata span. Stray, reversed or
// duplicate markers are conflicts, never guessed replacement boundaries.
func UpsertManagedBlock(content, block string) (string, error) {
	begins, ends := strings.Count(content, BlockBegin), strings.Count(content, BlockEnd)
	if begins == 0 && ends == 0 {
		separator := ""
		switch {
		case content == "", strings.HasSuffix(content, "\n\n"):
		case strings.HasSuffix(content, "\n"):
			separator = "\n"
		default:
			separator = "\n\n"
		}
		return content + separator + block + "\n", nil
	}
	begin, end := strings.Index(content, BlockBegin), strings.Index(content, BlockEnd)
	if begins != 1 || ends != 1 || end < begin {
		return "", fmt.Errorf("agent guidance file has malformed or duplicate Kata markers; expected one %q followed by one %q", BlockBegin, BlockEnd)
	}
	end += len(BlockEnd)
	return content[:begin] + block + content[end:], nil
}
