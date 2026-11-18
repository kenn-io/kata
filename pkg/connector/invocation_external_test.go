package connector_test

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	connector "go.kenn.io/kata/pkg/connector"
)

func TestInvocationContextSettingsCannotBeMutatedThroughSafeReflection(t *testing.T) {
	handler := &immutableInvocationHandler{}
	var output bytes.Buffer
	require.NoError(t, connector.ServeOne(
		t.Context(),
		bytes.NewBufferString(`{"protocol":"kata.connector.v1","id":"request-1","method":"describe","instance":"notes","settings":{"enabled":true},"params":{}}`),
		&output,
		handler,
	))

	assert.True(t, handler.immutable)
	assert.JSONEq(t, `{"enabled":true}`, string(handler.after.Settings))
}

type keyCapturingContext struct {
	context.Context
	key any
}

func (ctx *keyCapturingContext) Value(key any) any {
	ctx.key = key
	return ctx.Context.Value(key)
}

type immutableInvocationHandler struct {
	immutable bool
	after     connector.Invocation
}

func (h *immutableInvocationHandler) Describe(ctx context.Context, _ connector.DescribeParams) (connector.Description, *connector.Error) {
	forwarding := &keyCapturingContext{Context: ctx}
	before, ok := connector.InvocationFromContext(forwarding)
	if !ok {
		return connector.Description{}, &connector.Error{Code: "missing_invocation", Message: "missing invocation"}
	}

	mutateFirstByteSlice(reflect.ValueOf(ctx.Value(forwarding.key)))
	h.after, ok = connector.InvocationFromContext(ctx)
	h.immutable = ok && bytes.Equal(before.Settings, h.after.Settings)
	return connector.Description{}, nil
}

func mutateFirstByteSlice(value reflect.Value) bool {
	if !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return false
		}
		return mutateFirstByteSlice(value.Elem())
	case reflect.Struct:
		for _, field := range value.Fields() {
			if mutateFirstByteSlice(field) {
				return true
			}
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 && value.Len() > 0 {
			value.Bytes()[0] = '!'
			return true
		}
	}
	return false
}

func (*immutableInvocationHandler) ResolveRoot(context.Context, connector.ResolveRootParams) (connector.Root, *connector.Error) {
	return connector.Root{}, nil
}

func (*immutableInvocationHandler) ReadRoot(context.Context, connector.ReadRootParams) (connector.Root, *connector.Error) {
	return connector.Root{}, nil
}

func (*immutableInvocationHandler) ListComments(context.Context, connector.ListCommentsParams) (connector.ListCommentsResult, *connector.Error) {
	return connector.ListCommentsResult{}, nil
}

func (*immutableInvocationHandler) CompleteRoot(context.Context, connector.CompleteRootParams) (connector.Root, *connector.Error) {
	return connector.Root{}, nil
}

func (*immutableInvocationHandler) PublishComment(context.Context, connector.PublishCommentParams) (connector.Comment, *connector.Error) {
	return connector.Comment{}, nil
}

func (*immutableInvocationHandler) ListFields(context.Context, connector.ListFieldsParams) (connector.ListFieldsResult, *connector.Error) {
	return connector.ListFieldsResult{}, nil
}

func (*immutableInvocationHandler) ReadFields(context.Context, connector.ReadFieldsParams) (connector.ReadFieldsResult, *connector.Error) {
	return connector.ReadFieldsResult{}, nil
}

func (*immutableInvocationHandler) WriteFields(context.Context, connector.WriteFieldsParams) (connector.WriteFieldsResult, *connector.Error) {
	return connector.WriteFieldsResult{}, nil
}
