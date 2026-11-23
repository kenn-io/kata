package notification

import (
	"strings"

	"go.kenn.io/kata/internal/authorname"
)

// DisplayValue formats sender attribution in a copy of a notification.
func DisplayValue(value Value) Value {
	original := value.From
	value.From = authorname.Display(original)
	if original == value.From || value.Kind == "" || value.Re == "" || !strings.HasPrefix(value.Message, "latest: "+value.Kind+" ") {
		return value
	}
	old := " by " + Address(original, value.Teammate)
	replacement := " by " + Address(value.From, value.Teammate)
	if i := strings.Index(value.Message, old); i >= 0 {
		end := i + len(old)
		if end == len(value.Message) || strings.HasPrefix(value.Message[end:], ": ") {
			value.Message = value.Message[:i] + replacement + value.Message[end:]
		}
	}
	return value
}
