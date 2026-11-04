package db

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/cron"
)

type federationEventFeaturesKey struct{}

// WithFederationEventFeatures carries peer support into a relay read transaction.
// Internal callers without a peer use the local implementation's full support.
func WithFederationEventFeatures(ctx context.Context, supported string) context.Context {
	return context.WithValue(ctx, federationEventFeaturesKey{}, supported)
}

// RequireProjectEventFeatures fences peer reads before any relay progress write.
func RequireProjectEventFeatures(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) error {
	supported, peer := ctx.Value(federationEventFeaturesKey{}).(string)
	if !peer || RequireEventFeatures(supported, SupportedEventFeatures) == nil {
		return nil
	}
	required, err := ProjectRequiredEventFeatures(ctx, q, projectUID)
	if err != nil {
		return err
	}
	return RequireEventFeatures(supported, required)
}

// Current projections cover imports without events; retained payloads cover v2
// observations later replaced by v1. No process cache survives replacement state.
func projectSummaryRequired(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (bool, error) {
	const query = `SELECT e.type,e.payload FROM events e JOIN projects p ON p.id=e.project_id
WHERE p.uid=$1 AND e.type IN ('cron.run.observed','cron.run.snapshot') AND (
 e.payload LIKE '%"reason"%' OR e.payload LIKE '%"trigger"%' OR e.payload LIKE '%"usage"%' OR
 e.payload LIKE '%"steps"%' OR e.payload LIKE '%"log_ref"%' OR e.payload LIKE '%\u%' ESCAPE '!')
UNION ALL SELECT 'summary',r.summary_json FROM cron_runs r JOIN projects p ON p.id=r.project_id WHERE p.uid=$1 AND (
 r.summary_json LIKE '%"reason"%' OR r.summary_json LIKE '%"trigger"%' OR r.summary_json LIKE '%"usage"%' OR
 r.summary_json LIKE '%"steps"%' OR r.summary_json LIKE '%"log_ref"%' OR r.summary_json LIKE '%\u%' ESCAPE '!')`
	rows, err := q.QueryContext(ctx, query, projectUID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			return false, err
		}
		if kind == "summary" {
			summary, err := cron.ParseSummary([]byte(payload))
			if err != nil {
				return false, err
			}
			if summary.UsesV2() {
				return true, nil
			}
		} else {
			required, err := PayloadRequiredFeatures(kind, payload)
			if err != nil {
				return false, err
			}
			if RequireEventFeatures(CronEventFeature, required) != nil {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
