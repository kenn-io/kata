package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"iter"
	"strings"
)

// ImportKind identifies the owner-only portable policy record.
func (*ProjectCronAccessPolicy) ImportKind() string { return "project_cron_access_policy" }

// ExportCronAccessPolicies emits validated typed authority only for owner backups.
func ExportCronAccessPolicies(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) iter.Seq2[ImportRecord, error] {
	return func(yield func(ImportRecord, error) bool) {
		rows, err := q.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2 ORDER BY key`, len(CronAccessPolicyPrefix), CronAccessPolicyPrefix)
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				yield(nil, err)
				return
			}
			policy, err := DecodeCronAccessPolicy(strings.TrimPrefix(key, CronAccessPolicyPrefix), []byte(raw))
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(&policy, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(nil, err)
		}
	}
}

// ImportCronAccessPolicyTx restores an exact revision after project/team replay.
// The owner restore transaction already excludes concurrent policy writers.
func ImportCronAccessPolicyTx(ctx context.Context, tx Transaction, policy ProjectCronAccessPolicy) error {
	policy, err := NormalizeCronAccessPolicy(policy)
	if err != nil {
		return err
	}
	if policy.Revision < 1 {
		return errors.New("invalid backup cron policy revision")
	}
	if _, err := ReadCronAccessPolicy(ctx, tx, policy.ProjectUID); err != nil {
		return err
	}
	for _, grant := range []CronGrant{policy.Define, policy.Observe} {
		for _, team := range grant.TeamUIDs {
			var found string
			if err := tx.QueryRowContext(ctx, `SELECT uid FROM teams WHERE uid=$1`, team).Scan(&found); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(cronAccessDocument{Version: 1, Revision: policy.Revision, Define: policy.Define, Observe: policy.Observe})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, CronAccessPolicyPrefix+policy.ProjectUID, string(raw))
	return err
}

// DeleteAllCronAccessPoliciesTx removes target-only policy during owner restore.
func DeleteAllCronAccessPoliciesTx(ctx context.Context, tx Transaction) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE substr(key,1,$1)=$2`, len(CronAccessPolicyPrefix), CronAccessPolicyPrefix)
	return err
}
