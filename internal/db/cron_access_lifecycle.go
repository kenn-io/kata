package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"strings"
)

// RemoveCronAccessTeamTx removes a deleted team's UID from all local policies.
// The caller holds the exclusive access epoch lock and bumps that epoch once.
func RemoveCronAccessTeamTx(ctx context.Context, tx interface {
	Transaction
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, teamUID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2`, len(CronAccessPolicyPrefix), CronAccessPolicyPrefix)
	if err != nil {
		return err
	}
	var policies []ProjectCronAccessPolicy
	err = func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				return err
			}
			policy, err := DecodeCronAccessPolicy(strings.TrimPrefix(key, CronAccessPolicyPrefix), []byte(raw))
			if err != nil {
				return err
			}
			changed := false
			for _, grant := range []*CronGrant{&policy.Define, &policy.Observe} {
				if slices.Contains(grant.TeamUIDs, teamUID) {
					grant.TeamUIDs = slices.DeleteFunc(grant.TeamUIDs, func(value string) bool { return value == teamUID })
					changed = true
				}
			}
			if changed {
				if policy.Revision == math.MaxInt64 {
					return errors.New("cron policy revision exhausted")
				}
				policy.Revision++
				policies = append(policies, policy)
			}
		}
		return rows.Err()
	}()
	if err != nil {
		return err
	}
	for _, policy := range policies {
		raw, err := json.Marshal(cronAccessDocument{Version: 1, Revision: policy.Revision, Define: policy.Define, Observe: policy.Observe})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), CronAccessPolicyPrefix+policy.ProjectUID); err != nil {
			return err
		}
	}
	return nil
}

// CheckCronMergeAccessTx rejects a merge that broadens either local cron grant.
// Team-set containment is conservative: membership overlap does not establish
// future authority equivalence between two independent teams.
func CheckCronMergeAccessTx(ctx context.Context, tx Transaction, sourceUID, targetUID string) error {
	source, err := ReadCronAccessPolicy(ctx, tx, sourceUID)
	if err != nil {
		return err
	}
	target, err := ReadCronAccessPolicy(ctx, tx, targetUID)
	if err != nil {
		return err
	}
	noWider := func(source, target CronGrant) bool {
		if source.Mode == "any" {
			return true
		}
		if target.Mode != "teams" {
			return false
		}
		for _, team := range target.TeamUIDs {
			if !slices.Contains(source.TeamUIDs, team) {
				return false
			}
		}
		return true
	}
	if !noWider(source.Define, target.Define) || !noWider(source.Observe, target.Observe) {
		return ErrProjectMergeAccessPolicy
	}
	return nil
}

// DeleteCronAccessPolicyTx retires authority only on hard deletion or merge.
// Projection resets and ordinary archival retain it.
func DeleteCronAccessPolicyTx(ctx context.Context, tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, projectUID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, CronAccessPolicyPrefix+projectUID)
	return err
}

// RetargetCronAccessPolicyTx preserves this daemon's policy during an adopted
// project's UID rewrite. This does not import policy from the other daemon.
func RetargetCronAccessPolicyTx(ctx context.Context, tx Transaction, oldUID, newUID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE meta SET key=$1 WHERE key=$2`, CronAccessPolicyPrefix+newUID, CronAccessPolicyPrefix+oldUID)
	return err
}
