package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// addFederationRelayCapacityStatus reports, for each hub project this daemon
// roots that has a live relay enrollment, the signed checkpoint size it would
// build now and the capacity that bounds new relay enrollments and checkpoint
// creation. Measuring reads the project's retained history, so projects that
// never relay are not measured. A failed measurement is reported on that
// project instead of failing the status response.
func addFederationRelayCapacityStatus(ctx context.Context, cfg ServerConfig, body *api.FederationStatusBody) error {
	measurer, ok := cfg.DB.(db.RelayCheckpointMeasurer)
	if !ok {
		return nil
	}
	relayed, err := relayEnrolledProjects(ctx, cfg.DB)
	if err != nil {
		return internalAPIError(err)
	}
	for i := range body.Statuses {
		status := &body.Statuses[i]
		if status.Role != string(db.FederationRoleHub) || !relayed[status.ProjectID] {
			continue
		}
		pin, err := cfg.DB.RootAuthority(ctx, status.ProjectUID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && pin.AuthorityUID != cfg.DB.InstanceUID()) {
			continue
		}
		if err != nil {
			return internalAPIError(err)
		}
		size, err := measurer.RelayCheckpointSize(ctx, status.ProjectID)
		if err != nil {
			status.RelayCheckpointError = err.Error()
			continue
		}
		status.RelayCheckpointBytes = size.Bytes
		status.RelayCheckpointCapacity = size.Capacity
	}
	return nil
}

// relayEnrolledProjects lists the projects with a live relay enrollment.
func relayEnrolledProjects(ctx context.Context, store db.Storage) (map[int64]bool, error) {
	enrollments, err := store.ListFederationEnrollments(ctx)
	if err != nil {
		return nil, err
	}
	relayed := map[int64]bool{}
	for _, enrollment := range enrollments {
		if enrollment.RelayProtocolVersion == db.RelayProtocolVersion && enrollment.RevokedAt == nil && enrollment.ProjectID != nil {
			relayed[*enrollment.ProjectID] = true
		}
	}
	return relayed, nil
}
