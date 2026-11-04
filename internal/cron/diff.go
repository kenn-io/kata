package cron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Change describes one sequential field change using an RFC 6901 JSON Pointer.
// Missing sides are omitted; a literal JSON null is retained as a value.
type Change struct {
	Op     string         `json:"op"`
	Path   string         `json:"path"`
	Before jsontext.Value `json:"before,omitzero"`
	After  jsontext.Value `json:"after,omitzero"`
}

// DiffDocuments compares JSON objects by sorted key and arrays by index.
// Numbers compare lexically and retain their exact tokens; strings compare by
// decoded value. Array tail removals run backwards so changes apply in order.
func DiffDocuments(before, after jsontext.Value) ([]Change, error) {
	for _, value := range []jsontext.Value{before, after} {
		if !value.IsValid() {
			return nil, fmt.Errorf("%w: diff requires valid JSON", ErrInvalid)
		}
	}
	changes := []Change{}
	diffValues(before, after, "", &changes)
	return changes, nil
}

func diffValues(before, after jsontext.Value, path string, changes *[]Change) {
	if before.Kind() == '{' && after.Kind() == '{' {
		left, right := map[string]jsontext.Value{}, map[string]jsontext.Value{}
		_ = json.Unmarshal(before, &left)
		_ = json.Unmarshal(after, &right)
		keys := make([]string, 0, len(left)+len(right))
		for key := range left {
			keys = append(keys, key)
		}
		for key := range right {
			if _, ok := left[key]; !ok {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			old, had := left[key]
			next, has := right[key]
			pointer := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			switch {
			case !had:
				*changes = append(*changes, Change{Op: "add", Path: pointer, After: next})
			case !has:
				*changes = append(*changes, Change{Op: "remove", Path: pointer, Before: old})
			default:
				diffValues(old, next, pointer, changes)
			}
		}
		return
	}
	if before.Kind() == '[' && after.Kind() == '[' {
		left, right := []jsontext.Value{}, []jsontext.Value{}
		_ = json.Unmarshal(before, &left)
		_ = json.Unmarshal(after, &right)
		for i := 0; i < min(len(left), len(right)); i++ {
			diffValues(left[i], right[i], path+"/"+strconv.Itoa(i), changes)
		}
		for i := len(left) - 1; i >= len(right); i-- {
			*changes = append(*changes, Change{Op: "remove", Path: path + "/" + strconv.Itoa(i), Before: left[i]})
		}
		for i := len(left); i < len(right); i++ {
			*changes = append(*changes, Change{Op: "add", Path: path + "/" + strconv.Itoa(i), After: right[i]})
		}
		return
	}
	if before.Kind() == '"' && after.Kind() == '"' {
		var left, right string
		_ = json.Unmarshal(before, &left)
		_ = json.Unmarshal(after, &right)
		if left == right {
			return
		}
	} else {
		left, right := before.Clone(), after.Clone()
		_ = left.Compact()
		_ = right.Compact()
		if bytes.Equal(left, right) {
			return
		}
	}
	*changes = append(*changes, Change{Op: "replace", Path: path, Before: before.Clone(), After: after.Clone()})
}
