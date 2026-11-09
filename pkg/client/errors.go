package client

import (
	"errors"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/pkg/client/generated"
)

// StatusCode returns the HTTP status of the daemon response that produced
// err, including responses whose error body could not be decoded, such as a
// proxy's HTML error page. It returns 0 when err did not come from a response,
// for example a connection failure.
func StatusCode(err error) int {
	if apiErr, ok := errors.AsType[*runtime.ClientAPIError](err); ok {
		return apiErr.StatusCode()
	}
	if decodeErr, ok := errors.AsType[*runtime.ResponseDecodeError](err); ok {
		return decodeErr.StatusCode
	}
	return 0
}

// ErrorCode returns the structured daemon error class, when available, without
// parsing or exposing its message. A transport-only failure has no such class.
func ErrorCode(err error) string {
	if envelope, ok := errors.AsType[generated.ErrorEnvelope](err); ok {
		return envelope.ErrorData.Code
	}
	if envelope, ok := errors.AsType[*generated.ErrorEnvelope](err); ok {
		return envelope.ErrorData.Code
	}
	return ""
}
