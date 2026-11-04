package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/cron"
)

// RequireFederationCronGrant checks fresh downstream data using the authenticated
// hop actor, preserving source authorship. The caller holds the access epoch
// lock through commit and handles transport duplicates and adoption exemptions.
func RequireFederationCronGrant(ctx context.Context, tx Transaction, projectID int64, projectUID, actor string, event RemoteEvent) error {
	grant := ""
	switch {
	case event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot":
		grant = "observe"
	case strings.HasPrefix(event.Type, "cron.workflow.") && isCronDefinitionEvent(event.Type):
		grant = "define"
	case strings.HasPrefix(event.Type, "cron.job.") && isCronDefinitionEvent(event.Type):
		var incoming CronDefinitionEvent
		if err := json.Unmarshal(event.Payload, &incoming, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		next, err := cron.DecodeJob(incoming.Definition)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		var old *cron.JobDefinition
		current, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_jobs WHERE uid=$1 AND project_id=$2", incoming.UID, projectID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			previous, err := cron.DecodeJob(current.document)
			if err != nil {
				return err
			}
			old = &previous
		}
		required := cron.RequiresDefineGrant(old, &next)
		// Only an ordinary live update with the same identity can be a pure pause.
		if old != nil && (old.Action.Kind == "execute" || next.Action.Kind == "execute") && (event.Type != "cron.job.updated" || incoming.Name != current.Name || incoming.DeletedAt != nil || current.DeletedAt != nil) {
			required = true
		}
		if required {
			grant = "define"
		}
	default:
		return nil
	}
	if grant == "" {
		return nil
	}
	if err := RequireCronGrant(ctx, tx, projectUID, actor, grant); err != nil {
		if errors.Is(err, ErrCronPermissionDenied) {
			return fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
		}
		return err
	}
	return nil
}

var _ cronAccessQuery = (*sql.Tx)(nil)
