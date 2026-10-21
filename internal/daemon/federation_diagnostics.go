package daemon

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
)

// federationFailureTransformer observes the normalized API error, never the
// request or response bytes. In particular, streaming responses pass through
// without a writer wrapper or buffering.
func federationFailureTransformer(logger *slog.Logger) huma.Transformer {
	return func(ctx huma.Context, _ string, value any) (any, error) {
		op := ctx.Operation()
		failure, ok := value.(*api.APIError)
		if op == nil || (!strings.Contains(op.Path, "/federation/") && !strings.HasSuffix(op.Path, "/federation")) || !ok || failure.Status < 400 {
			return value, nil
		}
		cause := "request_rejected"
		switch failure.Status {
		case http.StatusUnauthorized:
			cause = "authentication"
		case http.StatusForbidden:
			cause = "authorization"
		case http.StatusNotFound:
			cause = "not_found"
		case http.StatusConflict:
			cause = "conflict"
		default:
			if failure.Status >= 500 {
				cause = "internal"
			}
		}
		logFederationFailure(logger, op.OperationID, failure.Status, failure.Code, cause)
		return value, nil
	}
}

// cause must be a fixed internal classification, never an arbitrary error
// message. operation and code come from the registered operation/API contract.
func logFederationFailure(logger *slog.Logger, operation string, status int, code string, cause string) {
	if logger == nil {
		logger = slog.Default()
	}
	args := []any{"operation", operation, "status", status, "code", code, "cause", cause}
	if status >= 500 {
		logger.Warn("federation request failed", args...)
	} else {
		logger.Debug("federation request failed", args...)
	}
}
