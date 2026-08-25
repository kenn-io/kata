package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProtocolServeOneRejectsInvalidRequestFraming(t *testing.T) {
	for _, tt := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "unknown protocol", input: `{"protocol":"unknown","id":"1","method":"describe"}`, want: "unsupported protocol"},
		{name: "unknown method", input: `{"protocol":"kata.connector.v1","id":"1","method":"unknown","instance":"notes","settings":{},"params":{}}`, want: "unknown method"},
		{name: "trailing JSON", input: `{"protocol":"kata.connector.v1","id":"1","method":"describe"} {}`, want: "trailing JSON"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := ServeOne(t.Context(), bytes.NewBufferString(tt.input), &output, protocolTestHandler{})
			require.ErrorContains(t, err, tt.want)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneDispatchesDescribe(t *testing.T) {
	var output bytes.Buffer
	err := ServeOne(t.Context(), bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"describe","instance":"notes","settings":{},"params":{}}`), &output, protocolTestHandler{})
	require.NoError(t, err)

	var response Response
	require.NoError(t, json.Unmarshal(output.Bytes(), &response))
	assert.Equal(t, ProtocolVersion, response.Protocol)
	assert.Equal(t, "request-1", response.ID)
	assert.JSONEq(t, `{"connector_id":"example.connector","display_name":"Example","protocol":"kata.connector.v1","capabilities":[],"account_identity":"account-1"}`, string(response.Result))
}

func TestProtocolServeOneRejectsMalformedEnvelopeBeforeHandler(t *testing.T) {
	for _, test := range []struct {
		name     string
		envelope string
	}{
		{name: "missing instance", envelope: `"settings":{}`},
		{name: "empty instance", envelope: `"instance":"","settings":{}`},
		{name: "padded instance", envelope: `"instance":" notes ","settings":{}`},
		{name: "missing settings", envelope: `"instance":"notes"`},
		{name: "null settings", envelope: `"instance":"notes","settings":null`},
		{name: "array settings", envelope: `"instance":"notes","settings":[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &describeCallHandler{}
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"describe",`+test.envelope+`,"params":{}}`),
				&output,
				handler,
			)
			require.ErrorContains(t, err, "connector request")
			assert.False(t, handler.called)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneRejectsMalformedParametersBeforeHandler(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
	}{
		{name: "missing", params: ""},
		{name: "null", params: `,"params":null`},
		{name: "unknown field", params: `,"params":{"root_key":"root-1","unexpected":true}`},
		{name: "missing root key", params: `,"params":{}`},
		{name: "empty root key", params: `,"params":{"root_key":""}`},
		{name: "padded root key", params: `,"params":{"root_key":" root-1 "}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &completeRootCallHandler{}
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"complete_root","instance":"notes","settings":{}`+test.params+`}`),
				&output,
				handler,
			)
			require.ErrorContains(t, err, "decode connector parameters")
			assert.False(t, handler.called)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneRejectsInvalidUTF8BeforeHandler(t *testing.T) {
	request := append(
		[]byte(`{"protocol":"kata.connector.v1","id":"request-1","method":"complete_root","instance":"notes","settings":{},"params":{"root_key":"root-`),
		0xff,
	)
	request = append(request, []byte(`"}}`)...)
	handler := &completeRootCallHandler{}
	var output bytes.Buffer
	err := ServeOne(t.Context(), bytes.NewReader(request), &output, handler)
	require.ErrorContains(t, err, "UTF-8")
	assert.False(t, handler.called)
	assert.Empty(t, output.String())
}

func TestProtocolServeOneRejectsInvalidRequiredParameters(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		params string
		want   string
	}{
		{name: "blank locator", method: "resolve_root", params: `{"locator":" "}`, want: "locator"},
		{name: "read root key", method: "read_root", params: `{"root_key":" root-1"}`, want: "root_key"},
		{name: "list comments root key", method: "list_comments", params: `{"root_key":""}`, want: "root_key"},
		{name: "publish root key", method: "publish_comment", params: `{"root_key":"root-1 ","body":"hello","operation_id":"operation-1"}`, want: "root_key"},
		{name: "missing field selectors", method: "read_fields", params: `{"root_key":"root-1"}`, want: "field_ids"},
		{name: "noncanonical field selector", method: "read_fields", params: `{"root_key":"root-1","field_ids":[" field-1"]}`, want: "field_id"},
		{name: "missing field values", method: "write_fields", params: `{"root_key":"root-1"}`, want: "fields"},
		{name: "noncanonical field key", method: "write_fields", params: `{"root_key":"root-1","fields":{"field-1 ":{"kind":"null"}}}`, want: "field_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"`+test.method+`","instance":"notes","settings":{},"params":`+test.params+`}`),
				&output,
				protocolTestHandler{},
			)
			require.ErrorContains(t, err, test.want)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneRejectsInvalidFieldValuesBeforeHandler(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "unsupported kind", value: `{"kind":"text","value":"hello"}`},
		{name: "malformed date", value: `{"kind":"date","value":"tomorrow"}`},
		{name: "non-UTC instant", value: `{"kind":"instant","value":"2026-08-20T10:00:00+00:00"}`},
		{name: "incomplete local datetime", value: `{"kind":"local_datetime","value":"2026-08-20T10:00:00"}`},
		{name: "nonempty null", value: `{"kind":"null","value":"unexpected"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &writeFieldsCallHandler{}
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"write_fields","instance":"notes","settings":{},"params":{"root_key":"root-1","fields":{"field-1":`+test.value+`}}}`),
				&output,
				handler,
			)
			require.ErrorContains(t, err, "field value")
			assert.False(t, handler.called)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneRejectsMismatchedWriteFieldKeysBeforeHandler(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
	}{
		{
			name: "missing expected field",
			params: `{"root_key":"root-1","fields":{"field-1":{"kind":"null"}},` +
				`"expected":{"field-2":{"kind":"null"}}}`,
		},
		{
			name: "extra expected field",
			params: `{"root_key":"root-1","fields":{"field-1":{"kind":"null"}},` +
				`"expected":{"field-1":{"kind":"null"},"field-2":{"kind":"null"}}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &writeFieldsCallHandler{}
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"write_fields","instance":"notes","settings":{},"params":`+test.params+`}`),
				&output,
				handler,
			)
			require.ErrorContains(t, err, "same fields")
			assert.False(t, handler.called)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneRequiresCanonicalPublicationOperationID(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
	}{
		{name: "missing", params: `{"root_key":"root-1","body":"hello"}`},
		{name: "empty", params: `{"root_key":"root-1","body":"hello","operation_id":""}`},
		{name: "padded", params: `{"root_key":"root-1","body":"hello","operation_id":" operation-1 "}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &publicationOperationHandler{}
			var output bytes.Buffer
			err := ServeOne(
				t.Context(),
				bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"publish_comment","instance":"notes","settings":{},"params":`+test.params+`}`),
				&output,
				handler,
			)
			require.ErrorContains(t, err, "operation_id")
			assert.False(t, handler.called)
			assert.Empty(t, output.String())
		})
	}
}

func TestProtocolServeOneDispatchesStablePublicationOperationID(t *testing.T) {
	handler := &publicationOperationHandler{}
	var output bytes.Buffer
	require.NoError(t, ServeOne(
		t.Context(),
		bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"publish_comment","instance":"notes","settings":{},"params":{"root_key":"root-1","body":"hello","operation_id":"publish-comment-1"}}`),
		&output,
		handler,
	))

	assert.True(t, handler.called)
	assert.Equal(t, "publish-comment-1", handler.operationID)
}

func TestProtocolServeOneAttachesImmutableInvocation(t *testing.T) {
	handler := &invocationTestHandler{}
	settings := json.RawMessage(`{"enabled":true}`)
	requests := []string{
		`{"protocol":"kata.connector.v1","id":"request-1","method":"describe","instance":"notes","settings":{"enabled":true},"params":{}}`,
		`{"protocol":"kata.connector.v1","id":"request-2","method":"read_root","instance":"notes","settings":{"enabled":true},"params":{"root_key":"root-1"}}`,
	}
	for _, request := range requests {
		var output bytes.Buffer
		require.NoError(t, ServeOne(t.Context(), bytes.NewBufferString(request), &output, handler))
	}
	require.Len(t, handler.invocations, 2)
	for _, invocation := range handler.invocations {
		assert.Equal(t, "notes", invocation.Instance)
		assert.JSONEq(t, string(settings), string(invocation.Settings))
	}
	assert.True(t, handler.immutable)
}

func TestInvocationFromContextRejectsForgedPublicInvocation(t *testing.T) {
	ctx := forgedInvocationContext{
		Context: t.Context(),
		invocation: Invocation{
			Instance: "forged",
			Settings: json.RawMessage(`{"forged":true}`),
		},
	}

	invocation, ok := InvocationFromContext(ctx)
	assert.False(t, ok)
	assert.Equal(t, Invocation{}, invocation)
}

func TestProtocolServeOneKeepsInvocationStoragePrivateFromForwardingContext(t *testing.T) {
	handler := &capturingInvocationHandler{}
	var output bytes.Buffer
	require.NoError(t, ServeOne(
		t.Context(),
		bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"describe","instance":"notes","settings":{"enabled":true},"params":{}}`),
		&output,
		handler,
	))

	assert.False(t, handler.storedAsPublicInvocation)
	assert.True(t, handler.storageImmutable)
	assert.Equal(t, "notes", handler.invocation.Instance)
	assert.JSONEq(t, `{"enabled":true}`, string(handler.invocation.Settings))
}

type protocolTestHandler struct{}

func (protocolTestHandler) Describe(context.Context, DescribeParams) (Description, *Error) {
	return Description{ConnectorID: "example.connector", DisplayName: "Example", Protocol: ProtocolVersion, Capabilities: []Capability{}, AccountIdentity: "account-1"}, nil
}

func (protocolTestHandler) ResolveRoot(context.Context, ResolveRootParams) (Root, *Error) {
	return Root{}, nil
}
func (protocolTestHandler) ReadRoot(context.Context, ReadRootParams) (Root, *Error) {
	return Root{}, nil
}
func (protocolTestHandler) ListComments(context.Context, ListCommentsParams) (ListCommentsResult, *Error) {
	return ListCommentsResult{}, nil
}
func (protocolTestHandler) CompleteRoot(context.Context, CompleteRootParams) (Root, *Error) {
	return Root{}, nil
}
func (protocolTestHandler) PublishComment(context.Context, PublishCommentParams) (Comment, *Error) {
	return Comment{}, nil
}
func (protocolTestHandler) ListFields(context.Context, ListFieldsParams) (ListFieldsResult, *Error) {
	return ListFieldsResult{}, nil
}
func (protocolTestHandler) ReadFields(context.Context, ReadFieldsParams) (ReadFieldsResult, *Error) {
	return ReadFieldsResult{}, nil
}
func (protocolTestHandler) WriteFields(context.Context, WriteFieldsParams) (WriteFieldsResult, *Error) {
	return WriteFieldsResult{}, nil
}

type describeCallHandler struct {
	protocolTestHandler
	called bool
}

func (h *describeCallHandler) Describe(context.Context, DescribeParams) (Description, *Error) {
	h.called = true
	return h.protocolTestHandler.Describe(context.Background(), DescribeParams{})
}

type publicationOperationHandler struct {
	protocolTestHandler
	called      bool
	operationID string
}

type completeRootCallHandler struct {
	protocolTestHandler
	called bool
}

type writeFieldsCallHandler struct {
	protocolTestHandler
	called bool
}

func (h *writeFieldsCallHandler) WriteFields(context.Context, WriteFieldsParams) (WriteFieldsResult, *Error) {
	h.called = true
	return WriteFieldsResult{}, nil
}

func (h *completeRootCallHandler) CompleteRoot(context.Context, CompleteRootParams) (Root, *Error) {
	h.called = true
	return Root{}, nil
}

func (h *publicationOperationHandler) PublishComment(_ context.Context, params PublishCommentParams) (Comment, *Error) {
	h.called = true
	h.operationID = params.OperationID
	return Comment{}, nil
}

type forgedInvocationContext struct {
	context.Context
	invocation Invocation
}

func (ctx forgedInvocationContext) Value(any) any {
	return ctx.invocation
}

type invocationKeyCapturingContext struct {
	context.Context
	key any
}

func (ctx *invocationKeyCapturingContext) Value(key any) any {
	ctx.key = key
	return ctx.Context.Value(key)
}

type capturingInvocationHandler struct {
	protocolTestHandler
	invocation               Invocation
	storedAsPublicInvocation bool
	storageImmutable         bool
}

func (h *capturingInvocationHandler) Describe(ctx context.Context, _ DescribeParams) (Description, *Error) {
	forwarding := &invocationKeyCapturingContext{Context: ctx}
	invocation, ok := InvocationFromContext(forwarding)
	if !ok {
		return Description{}, &Error{Code: "missing_invocation", Message: "missing invocation"}
	}
	h.invocation = invocation

	stored, public := ctx.Value(forwarding.key).(Invocation)
	h.storedAsPublicInvocation = public
	if public && len(stored.Settings) > 0 {
		stored.Settings[0] = '!'
	}
	again, ok := InvocationFromContext(ctx)
	h.storageImmutable = ok && bytes.Equal(again.Settings, invocation.Settings)
	return Description{}, nil
}

type invocationTestHandler struct {
	invocations []Invocation
	immutable   bool
}

func (h *invocationTestHandler) capture(ctx context.Context) *Error {
	invocation, ok := InvocationFromContext(ctx)
	if !ok {
		return &Error{Code: "missing_invocation", Message: "missing invocation"}
	}
	h.invocations = append(h.invocations, Invocation{
		Instance: invocation.Instance,
		Settings: append(json.RawMessage(nil), invocation.Settings...),
	})
	if len(h.invocations) == 1 {
		h.immutable = true
	}
	if len(invocation.Settings) > 0 {
		invocation.Settings[0] = '!'
	}
	again, ok := InvocationFromContext(ctx)
	h.immutable = h.immutable && ok && bytes.Equal(again.Settings, h.invocations[len(h.invocations)-1].Settings)
	return nil
}

func (h *invocationTestHandler) Describe(ctx context.Context, _ DescribeParams) (Description, *Error) {
	if err := h.capture(ctx); err != nil {
		return Description{}, err
	}
	return Description{}, nil
}

func (h *invocationTestHandler) ResolveRoot(context.Context, ResolveRootParams) (Root, *Error) {
	return Root{}, nil
}
func (h *invocationTestHandler) ReadRoot(ctx context.Context, _ ReadRootParams) (Root, *Error) {
	if err := h.capture(ctx); err != nil {
		return Root{}, err
	}
	return Root{}, nil
}
func (h *invocationTestHandler) ListComments(context.Context, ListCommentsParams) (ListCommentsResult, *Error) {
	return ListCommentsResult{}, nil
}
func (h *invocationTestHandler) CompleteRoot(context.Context, CompleteRootParams) (Root, *Error) {
	return Root{}, nil
}
func (h *invocationTestHandler) PublishComment(context.Context, PublishCommentParams) (Comment, *Error) {
	return Comment{}, nil
}
func (h *invocationTestHandler) ListFields(context.Context, ListFieldsParams) (ListFieldsResult, *Error) {
	return ListFieldsResult{}, nil
}
func (h *invocationTestHandler) ReadFields(context.Context, ReadFieldsParams) (ReadFieldsResult, *Error) {
	return ReadFieldsResult{}, nil
}
func (h *invocationTestHandler) WriteFields(context.Context, WriteFieldsParams) (WriteFieldsResult, *Error) {
	return WriteFieldsResult{}, nil
}
