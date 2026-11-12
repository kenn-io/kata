package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// UnmarshalJSON keeps malformed items separate from their valid neighbors.
func (item *MoveIssuesItem) UnmarshalJSON(data []byte) error {
	var members map[string]jsontext.Value
	_ = json.Unmarshal(data, &members)
	var ref string
	if raw, ok := members["ref"]; ok {
		_ = json.Unmarshal(raw, &ref)
	}
	type fields MoveIssuesItem
	var decoded fields
	if err := json.Unmarshal(data, &decoded, json.RejectUnknownMembers(true)); err != nil {
		*item = MoveIssuesItem{Ref: ref, Invalid: true}
		return nil
	}
	for _, value := range members {
		if value.Kind() == 'n' {
			*item = MoveIssuesItem{Ref: ref, Invalid: true}
			return nil
		}
	}
	*item = MoveIssuesItem(decoded)
	return nil
}

// UnmarshalJSON rejects unsupported request fields while items validate separately.
func (body *MoveIssuesBody) UnmarshalJSON(data []byte) error {
	type fields MoveIssuesBody
	return json.Unmarshal(data, (*fields)(body), json.RejectUnknownMembers(true))
}
