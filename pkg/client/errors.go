package client

import (
	"errors"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
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
