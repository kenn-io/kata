package notification

import "encoding/json/v2"

// ParseValue decodes a notify.* metadata value for inbox reads and event matching.
func ParseValue(raw any) (Value, bool) {
	b, err := json.Marshal(raw)
	if err != nil {
		return Value{}, false
	}
	value, _, err := DecodeValue(b)
	return value, err == nil
}
