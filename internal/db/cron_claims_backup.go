package db

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
)

// CronClaimExport is owner-only control state, excluded from project transfer.
type CronClaimExport CronOccurrenceClaim

// ImportKind identifies a retained reservation in an owner backup.
func (*CronClaimExport) ImportKind() string { return "cron_occurrence_claim" }

// ExportCronClaimsSQL uses the backend's consistent export snapshot.
func ExportCronClaimsSQL(ctx context.Context, query CronQuery, filter ExportFilter) iter.Seq2[CronClaimExport, error] {
	return cronExport(ctx, query, filter, "cron_occurrence_claims", cronClaimColumns, false, func(rows *sql.Rows) (CronClaimExport, error) {
		c, e := scanCronClaim(rows)
		return CronClaimExport(c), e
	})
}

// ReplayCronClaim preserves the exclusion rather than inventing a new decision.
func ReplayCronClaim(ctx context.Context, tx *sql.Tx, c CronOccurrenceClaim) error {
	if err := ValidateCronClaim(c); err != nil {
		return err
	}
	if err := insertCronClaim(ctx, tx, c); err != nil {
		return fmt.Errorf("restore occurrence reservation: %w", err)
	}
	return nil
}
