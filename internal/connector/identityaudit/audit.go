// Package identityaudit validates provider-facing connector parameters for
// local Kata identity channels.
package identityaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"go.kenn.io/kata/pkg/connector"
)

// Code identifies a connector parameter validation failure.
type Code string

// Error codes returned by Validate.
const (
	CodeInvalidJSON       Code = "invalid_json"
	CodeTrailingJSON      Code = "trailing_json"
	CodeForbiddenKey      Code = "forbidden_key"
	CodeUnsupportedMethod Code = "unsupported_method"
	CodeMissingParameter  Code = "missing_parameter"
	CodeUnknownParameter  Code = "unknown_parameter"
	CodeWrongRoot         Code = "wrong_root"
	CodeLocalIdentity     Code = "local_identity"
)

// Error is a structured connector parameter validation failure.
type Error struct {
	Code   Code
	Method string
	Path   string
	Cause  error
}

func (e *Error) Error() string {
	message := fmt.Sprintf("%s parameter audit failed", e.Method)
	if e.Path != "" {
		message += " at " + e.Path
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap returns the underlying JSON or context failure, when present.
func (e *Error) Unwrap() error { return e.Cause }

// Options supplies the expected external root and local issue identities.
// Long UIDs are rejected as substrings; short IDs are rejected only as exact
// string values.
type Options struct {
	ExternalRootKey string
	LongUIDs        []string
	ShortIDs        []string
}

// Validate checks one raw protocol parameter object and requires exactly one
// JSON value followed by EOF.
func Validate(method string, raw json.RawMessage, options Options) error {
	var params map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil {
		return &Error{Code: CodeInvalidJSON, Method: method, Path: "params", Cause: err}
	}
	if params == nil {
		return &Error{Code: CodeInvalidJSON, Method: method, Path: "params", Cause: errors.New("parameters must be a JSON object")}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &Error{Code: CodeTrailingJSON, Method: method, Path: "params", Cause: err}
	}
	if path, found := forbiddenPath(params, "params", method); found {
		return &Error{Code: CodeForbiddenKey, Method: method, Path: path}
	}
	allowed, ok := methodParameters[method]
	if !ok {
		return &Error{Code: CodeUnsupportedMethod, Method: method}
	}
	for required := range allowed {
		if _, present := params[required]; !present {
			return &Error{Code: CodeMissingParameter, Method: method, Path: "params." + required}
		}
	}
	for key := range params {
		if !allowed[key] {
			return &Error{Code: CodeUnknownParameter, Method: method, Path: "params." + key}
		}
	}
	if err := validateParameterTypes(method, raw, params); err != nil {
		return err
	}
	if rootKey, present := params["root_key"]; present && rootKey != options.ExternalRootKey {
		return &Error{Code: CodeWrongRoot, Method: method, Path: "params.root_key"}
	}
	if path, found := localIdentityPath(params, "params", method, options); found {
		return &Error{Code: CodeLocalIdentity, Method: method, Path: path}
	}
	return nil
}

func validateParameterTypes(method string, raw json.RawMessage, params map[string]any) error {
	var target any
	switch method {
	case "describe":
		target = &connector.DescribeParams{}
	case "resolve_root":
		target = &connector.ResolveRootParams{}
	case "read_root":
		target = &connector.ReadRootParams{}
	case "list_comments":
		target = &connector.ListCommentsParams{}
	case "publish_comment":
		target = &connector.PublishCommentParams{}
	case "complete_root":
		target = &connector.CompleteRootParams{}
	case "list_fields":
		target = &connector.ListFieldsParams{}
	case "read_fields":
		target = &connector.ReadFieldsParams{}
	case "write_fields":
		target = &connector.WriteFieldsParams{}
	default:
		return &Error{Code: CodeUnsupportedMethod, Method: method}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &Error{Code: CodeInvalidJSON, Method: method, Path: "params", Cause: err}
	}

	invalid := func(path string) error {
		return &Error{
			Code:   CodeInvalidJSON,
			Method: method,
			Path:   path,
			Cause:  errors.New("parameter value has wrong JSON type"),
		}
	}
	requireString := func(key string) error {
		if _, ok := params[key].(string); !ok {
			return invalid("params." + key)
		}
		return nil
	}
	switch method {
	case "resolve_root":
		return requireString("locator")
	case "read_root", "list_comments", "complete_root":
		return requireString("root_key")
	case "publish_comment":
		if err := requireString("root_key"); err != nil {
			return err
		}
		if err := requireString("body"); err != nil {
			return err
		}
		return requireString("operation_id")
	case "read_fields":
		if err := requireString("root_key"); err != nil {
			return err
		}
		fieldIDs, ok := params["field_ids"].([]any)
		if !ok {
			return invalid("params.field_ids")
		}
		for index, fieldID := range fieldIDs {
			if _, ok := fieldID.(string); !ok {
				return invalid(fmt.Sprintf("params.field_ids[%d]", index))
			}
		}
	case "write_fields":
		if err := requireString("root_key"); err != nil {
			return err
		}
		fields, ok := params["fields"].(map[string]any)
		if !ok {
			return invalid("params.fields")
		}
		for fieldID, value := range fields {
			field, ok := value.(map[string]any)
			if !ok {
				return invalid("params.fields." + fieldID)
			}
			if _, ok := field["kind"].(string); !ok {
				return invalid("params.fields." + fieldID + ".kind")
			}
			for _, optional := range []string{"value", "timezone"} {
				if rawValue, present := field[optional]; present {
					if _, ok := rawValue.(string); !ok {
						return invalid("params.fields." + fieldID + "." + optional)
					}
				}
			}
		}
	}
	return nil
}

var methodParameters = map[string]map[string]bool{
	"describe":        {},
	"resolve_root":    {"locator": true},
	"read_root":       {"root_key": true},
	"list_comments":   {"root_key": true},
	"publish_comment": {"root_key": true, "body": true, "operation_id": true},
	"complete_root":   {"root_key": true},
	"list_fields":     {},
	"read_fields":     {"root_key": true, "field_ids": true},
	"write_fields":    {"root_key": true, "fields": true},
}

func forbiddenPath(value any, path, method string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if method == "write_fields" && path == "params" && key == "fields" {
				if fields, ok := child.(map[string]any); ok {
					for fieldID, fieldValue := range fields {
						if found, ok := forbiddenPath(fieldValue, path+".fields."+fieldID, method); ok {
							return found, true
						}
					}
					continue
				}
			}
			if method == "read_fields" && path == "params" && key == "field_ids" {
				continue
			}
			if forbiddenSemanticKey(key) {
				return path + "." + key, true
			}
			if found, ok := forbiddenPath(child, path+"."+key, method); ok {
				return found, true
			}
		}
	case []any:
		for index, child := range typed {
			if found, ok := forbiddenPath(child, fmt.Sprintf("%s[%d]", path, index), method); ok {
				return found, true
			}
		}
	}
	return "", false
}

func forbiddenSemanticKey(key string) bool {
	normalized := normalizeSemanticKey(key)
	if normalized == "kata" || strings.HasPrefix(normalized, "kata_") {
		return true
	}
	switch normalized {
	case "issue_id", "issue_uid", "issue_short_id", "child_root", "child_issue", "work_attention", "work_attention_msg":
		return true
	default:
		return false
	}
}

func normalizeSemanticKey(key string) string {
	runes := []rune(key)
	var normalized strings.Builder
	separator := false
	for index, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			separator = normalized.Len() > 0
			continue
		}
		if unicode.IsUpper(current) && index > 0 {
			previous := runes[index-1]
			var next rune
			if index+1 < len(runes) {
				next = runes[index+1]
			}
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && unicode.IsLower(next)) {
				separator = true
			}
		}
		if separator && normalized.Len() > 0 {
			normalized.WriteByte('_')
		}
		separator = false
		normalized.WriteRune(unicode.ToLower(current))
	}
	return normalized.String()
}

func localIdentityPath(value any, path, method string, options Options) (string, bool) {
	switch typed := value.(type) {
	case string:
		for _, uid := range options.LongUIDs {
			if uid != "" && strings.Contains(typed, uid) {
				return path, true
			}
		}
		for _, shortID := range options.ShortIDs {
			if shortID != "" && typed == shortID {
				return path, true
			}
		}
	case map[string]any:
		for key, child := range typed {
			if method == "read_fields" && path == "params" && key == "field_ids" {
				continue
			}
			if found, ok := localIdentityPath(child, path+"."+key, method, options); ok {
				return found, true
			}
		}
	case []any:
		for index, child := range typed {
			if found, ok := localIdentityPath(child, fmt.Sprintf("%s[%d]", path, index), method, options); ok {
				return found, true
			}
		}
	}
	return "", false
}
