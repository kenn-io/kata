package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// ParseValue decodes a notify.* metadata value. Current inbox reads and event
// matching share it, so both agree on which requests are valid. Bad teammate
// metadata is ignored without hiding an otherwise valid request.
func ParseValue(raw any) (Value, bool) {
	b, err := json.Marshal(raw)
	if err != nil {
		return Value{}, false
	}
	value, _, err := DecodeValue(jsontext.Value(b))
	return value, err == nil
}
