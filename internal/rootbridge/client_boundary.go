package rootbridge

import (
	"context"
	"errors"
	"strings"

	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/pkg/connector"
)

// ErrConnectorCall marks a transport or protocol failure at the configured
// connector boundary. Its fixed message is safe for durable failure state.
var ErrConnectorCall = errors.New("external connector request failed")

type connectorCallError struct {
	cause error
}

func (e *connectorCallError) Error() string { return safeConnectorCallMessage(e.cause) }

func (e *connectorCallError) Unwrap() []error {
	return []error{ErrConnectorCall, e.cause}
}

func safeConnectorCallMessage(err error) string {
	switch {
	case errors.Is(err, connectorclient.ErrRequestTimeout):
		return connectorclient.ErrRequestTimeout.Error()
	case errors.Is(err, connectorclient.ErrProtocolFailure):
		return connectorclient.ErrProtocolFailure.Error()
	case errors.Is(err, connectorclient.ErrProcessFailure):
		return connectorclient.ErrProcessFailure.Error()
	default:
		return ErrConnectorCall.Error()
	}
}

func connectorProtocolFailure() error {
	return &connectorCallError{cause: connectorclient.ErrProtocolFailure}
}

type boundaryClient struct {
	client connectorclient.Client
}

func wrapConnectorClient(client connectorclient.Client) connectorclient.Client {
	return &boundaryClient{client: client}
}

func connectorBoundaryError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if hasConnectorCategory(err) || errors.Is(err, ErrConnectorCall) {
		return &connectorCallError{cause: err}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if _, ok := errors.AsType[*connector.Error](err); ok {
		return err
	}
	return &connectorCallError{cause: err}
}

func parentContextEnded(ctx context.Context, err error) bool {
	if hasConnectorFailureProvenance(err) {
		return false
	}
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}

func hasConnectorFailureProvenance(err error) bool {
	if errors.Is(err, ErrConnectorCall) || hasConnectorCategory(err) {
		return true
	}
	_, ok := errors.AsType[*connector.Error](err)
	return ok
}

func hasConnectorCategory(err error) bool {
	return errors.Is(err, connectorclient.ErrRequestTimeout) ||
		errors.Is(err, connectorclient.ErrProtocolFailure) ||
		errors.Is(err, connectorclient.ErrProcessFailure)
}

func (c *boundaryClient) Describe(ctx context.Context) (connector.Description, error) {
	result, err := c.client.Describe(ctx)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) ResolveRoot(ctx context.Context, params connector.ResolveRootParams) (connector.Root, error) {
	result, err := c.client.ResolveRoot(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) ReadRoot(ctx context.Context, params connector.ReadRootParams) (connector.Root, error) {
	result, err := c.client.ReadRoot(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) ListComments(ctx context.Context, params connector.ListCommentsParams) (connector.ListCommentsResult, error) {
	result, err := c.client.ListComments(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) CompleteRoot(ctx context.Context, params connector.CompleteRootParams) (connector.Root, error) {
	result, err := c.client.CompleteRoot(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) PublishComment(ctx context.Context, params connector.PublishCommentParams) (connector.Comment, error) {
	result, err := c.client.PublishComment(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) ListFields(ctx context.Context) (connector.ListFieldsResult, error) {
	result, err := c.client.ListFields(ctx)
	if err := connectorBoundaryError(ctx, err); err != nil {
		return result, err
	}
	for _, descriptor := range result.Fields {
		if descriptor.ID == "" || strings.TrimSpace(descriptor.ID) != descriptor.ID ||
			descriptor.SchemaRevision == "" || strings.TrimSpace(descriptor.SchemaRevision) != descriptor.SchemaRevision {
			return connector.ListFieldsResult{}, connectorProtocolFailure()
		}
	}
	return result, nil
}

func (c *boundaryClient) ReadFields(ctx context.Context, params connector.ReadFieldsParams) (connector.ReadFieldsResult, error) {
	result, err := c.client.ReadFields(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}

func (c *boundaryClient) WriteFields(ctx context.Context, params connector.WriteFieldsParams) (connector.WriteFieldsResult, error) {
	result, err := c.client.WriteFields(ctx, params)
	return result, connectorBoundaryError(ctx, err)
}
