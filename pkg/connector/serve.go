package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// ServeOne reads, validates, dispatches, and responds to one protocol request.
func ServeOne(ctx context.Context, in io.Reader, out io.Writer, handler Handler) error {
	if handler == nil {
		return errors.New("connector handler is nil")
	}
	encoded, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read connector request: %w", err)
	}
	if !utf8.Valid(encoded) {
		return errors.New("decode connector request: request is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode connector request: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("connector request contains trailing JSON")
		}
		return fmt.Errorf("decode connector request trailing JSON: %w", err)
	}
	if request.Protocol != ProtocolVersion {
		return fmt.Errorf("unsupported protocol %q", request.Protocol)
	}
	if err := validateRequestEnvelope(request); err != nil {
		return fmt.Errorf("invalid connector request: %w", err)
	}
	ctx = withInvocation(ctx, request)

	result, callErr, err := dispatch(ctx, handler, request)
	if err != nil {
		return err
	}
	response := Response{Protocol: ProtocolVersion, ID: request.ID, Result: result, Error: callErr}
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return fmt.Errorf("encode connector response: %w", err)
	}
	return nil
}

func validateRequestEnvelope(request Request) error {
	if err := validateCanonicalIdentifier("instance", request.Instance); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(request.Settings)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("settings must be a JSON object")
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &settings); err != nil || settings == nil {
		return errors.New("settings must be a JSON object")
	}
	return nil
}

func dispatch(ctx context.Context, handler Handler, request Request) (json.RawMessage, *Error, error) {
	switch request.Method {
	case "describe":
		return call(request.Params, func(params DescribeParams) (Description, *Error) { return handler.Describe(ctx, params) })
	case "resolve_root":
		return callValidated(request.Params, validateResolveRootParams, func(params ResolveRootParams) (Root, *Error) { return handler.ResolveRoot(ctx, params) })
	case "read_root":
		return callValidated(request.Params, func(params ReadRootParams) error { return validateRootKey(params.RootKey) }, func(params ReadRootParams) (Root, *Error) { return handler.ReadRoot(ctx, params) })
	case "list_comments":
		return callValidated(request.Params, func(params ListCommentsParams) error { return validateRootKey(params.RootKey) }, func(params ListCommentsParams) (ListCommentsResult, *Error) { return handler.ListComments(ctx, params) })
	case "complete_root":
		return callValidated(request.Params, func(params CompleteRootParams) error { return validateRootKey(params.RootKey) }, func(params CompleteRootParams) (Root, *Error) { return handler.CompleteRoot(ctx, params) })
	case "publish_comment":
		return callValidated(request.Params, func(params PublishCommentParams) error {
			if err := validateRootKey(params.RootKey); err != nil {
				return err
			}
			if !ValidOperationID(params.OperationID) {
				return errors.New("operation_id must be nonempty and canonical")
			}
			return nil
		}, func(params PublishCommentParams) (Comment, *Error) { return handler.PublishComment(ctx, params) })
	case "list_fields":
		return call(request.Params, func(params ListFieldsParams) (ListFieldsResult, *Error) { return handler.ListFields(ctx, params) })
	case "read_fields":
		return callValidated(request.Params, validateReadFieldsParams, func(params ReadFieldsParams) (ReadFieldsResult, *Error) { return handler.ReadFields(ctx, params) })
	case "write_fields":
		return callValidated(request.Params, validateWriteFieldsParams, func(params WriteFieldsParams) (WriteFieldsResult, *Error) { return handler.WriteFields(ctx, params) })
	default:
		return nil, nil, fmt.Errorf("unknown method %q", request.Method)
	}
}

func call[P any, R any](raw json.RawMessage, fn func(P) (R, *Error)) (json.RawMessage, *Error, error) {
	return callValidated(raw, nil, fn)
}

func callValidated[P any, R any](raw json.RawMessage, validate func(P) error, fn func(P) (R, *Error)) (json.RawMessage, *Error, error) {
	var params P
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, errors.New("decode connector parameters: parameters must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return nil, nil, fmt.Errorf("decode connector parameters: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, nil, errors.New("decode connector parameters: trailing JSON")
		}
		return nil, nil, fmt.Errorf("decode connector parameters: trailing JSON: %w", err)
	}
	if validate != nil {
		if err := validate(params); err != nil {
			return nil, nil, fmt.Errorf("decode connector parameters: %w", err)
		}
	}
	result, callErr := fn(params)
	if callErr != nil {
		return nil, callErr, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, fmt.Errorf("encode connector result: %w", err)
	}
	return encoded, nil, nil
}

func validateResolveRootParams(params ResolveRootParams) error {
	if strings.TrimSpace(params.Locator) == "" {
		return errors.New("locator must be nonempty")
	}
	return nil
}

func validateRootKey(rootKey string) error {
	return validateCanonicalIdentifier("root_key", rootKey)
}

func validateReadFieldsParams(params ReadFieldsParams) error {
	if err := validateRootKey(params.RootKey); err != nil {
		return err
	}
	if params.FieldIDs == nil {
		return errors.New("field_ids must be present")
	}
	for _, fieldID := range params.FieldIDs {
		if err := validateCanonicalIdentifier("field_id", fieldID); err != nil {
			return err
		}
	}
	return nil
}

func validateWriteFieldsParams(params WriteFieldsParams) error {
	if err := validateRootKey(params.RootKey); err != nil {
		return err
	}
	if params.Fields == nil {
		return errors.New("fields must be present")
	}
	for fieldID, value := range params.Fields {
		if err := validateCanonicalIdentifier("field_id", fieldID); err != nil {
			return err
		}
		if err := ValidateFieldValue(value); err != nil {
			return err
		}
	}
	return nil
}

func validateCanonicalIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be nonempty and canonical", name)
	}
	return nil
}
