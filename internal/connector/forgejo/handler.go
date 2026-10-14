// Package forgejo implements the external-root connector for Forgejo issues.
package forgejo

import (
	"context"

	"go.kenn.io/kata/pkg/connector"
)

type handler struct{ token string }

// New returns a Forgejo connector. Invocation settings supply its origin.
func New(token string) connector.Handler { return &handler{token: token} }
func unsupported() *connector.Error {
	return &connector.Error{Code: "unsupported", Message: "Operation is unsupported"}
}
func (h *handler) Describe(ctx context.Context, _ connector.DescribeParams) (connector.Description, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Description{}, err
	}
	self, err := c.self()
	if err != nil {
		return connector.Description{}, err
	}
	return connector.Description{ConnectorID: "forgejo", DisplayName: "Forgejo", Protocol: connector.ProtocolVersion,
		Capabilities: []connector.Capability{connector.CapabilityPublishComment, connector.CapabilityFields}, SelfActorID: id(self.ID), AccountIdentity: c.origin,
		ConfigSchema: []byte(`{"type":"object","properties":{"origin":{"type":"string"}},"required":["origin"],"additionalProperties":false}`)}, nil
}
