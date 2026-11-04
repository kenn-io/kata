package tui

import (
	"context"
	"time"

	"go.kenn.io/kata/pkg/client/generated"
)

type cronReplica struct {
	role              string
	pulledAt, errorAt *time.Time
	lastError         string
}

// Status is optional for older peers and test readers; failure omits the chip.
type cronReplicaAPI interface {
	readCronReplica(context.Context, int64) (cronReplica, error)
}

func (c *Client) readCronReplica(ctx context.Context, projectID int64) (cronReplica, error) {
	client, err := c.generatedClient()
	if err != nil {
		return cronReplica{}, err
	}
	wire, callErr := client.GetProjectFederationStatusWithResponse(ctx, &generated.GetProjectFederationStatusRequestOptions{
		PathParams: &generated.GetProjectFederationStatusPath{ProjectID: projectID},
	})
	if wire == nil {
		return cronReplica{}, callErr
	}
	if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
		return cronReplica{}, err
	}
	for _, status := range wire.JSON200.Statuses {
		if status.ProjectID == projectID {
			return cronReplica{role: status.Role, pulledAt: status.LastPullSuccessAt, errorAt: status.LastErrorAt, lastError: deref(status.LastError)}, nil
		}
	}
	return cronReplica{}, nil
}

func cronReplicaChip(p cronReplica) string {
	if p.role != "spoke" {
		return ""
	}
	if p.lastError != "" && p.errorAt != nil && (p.pulledAt == nil || p.errorAt.After(*p.pulledAt)) {
		return errorStyle.Render("replica · sync error " + humanizeRelative(*p.errorAt))
	}
	if p.pulledAt == nil {
		return "replica · never pulled"
	}
	return "replica · pulled " + humanizeRelative(*p.pulledAt)
}
