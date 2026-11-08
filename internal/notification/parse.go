package notification

import (
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/teammate"
)

// ParseValue decodes a notify.* metadata value. Current inbox reads and event
// matching share it, so both agree on which requests are valid. Bad teammate
// metadata is ignored without hiding an otherwise valid request.
func ParseValue(raw any) (Value, bool) {
	var value struct {
		From     string `json:"from"`
		Message  string `json:"message"`
		Teammate any    `json:"teammate"`
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &value) != nil || strings.TrimSpace(value.From) == "" || strings.TrimSpace(value.Message) == "" {
		return Value{}, false
	}
	handle, _ := value.Teammate.(string)
	if teammate.Validate(handle) != nil {
		handle = ""
	}
	return Value{From: value.From, Message: value.Message, Teammate: handle}, true
}
