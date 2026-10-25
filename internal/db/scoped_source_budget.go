package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

type scopedSourceBudgetUsage struct {
	FamilyBytes   map[string]int64
	BindingBytes  map[string]int64
	PrivateBytes  map[string]int64
	ActiveViews   map[string]int
	OriginalBytes map[string]int64
	neighbors     map[string][]string
}

func newScopedSourceBudgetUsage() scopedSourceBudgetUsage {
	return scopedSourceBudgetUsage{
		FamilyBytes:   map[string]int64{},
		BindingBytes:  map[string]int64{},
		PrivateBytes:  map[string]int64{},
		ActiveViews:   map[string]int{},
		OriginalBytes: map[string]int64{},
		neighbors:     map[string][]string{},
	}
}

const scopedBudgetViewBytes = `COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.catalog_entry_uid),0) +
	COALESCE(octet_length(r.authority_uid),0) +
	COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.accountable_actor),0) +
	COALESCE(octet_length(r.route_digest),0) +
	COALESCE(octet_length(r.destination_uid),0) +
	COALESCE(octet_length(r.state),0) +
	COALESCE(octet_length(r.reason_code),0) +
	COALESCE(octet_length(r.request_digest),0) +
	8 + CASE WHEN r.active_generation IS NULL THEN 0 ELSE 8 END + CASE WHEN r.prepared_generation IS NULL THEN 0 ELSE 8 END`

const scopedBudgetGenerationBytes = `COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.descriptor_bytes),0) +
	COALESCE(octet_length(r.descriptor_digest),0) +
	COALESCE(octet_length(r.checkpoint_bytes),0) +
	COALESCE(octet_length(r.checkpoint_digest),0) +
	COALESCE(octet_length(r.cut_digest),0) +
	COALESCE(octet_length(r.state),0) +
	COALESCE(octet_length(r.root_cutover_uid),0) +
	24`

const scopedBudgetProjectBytes = `COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.owner_view_uid),0) +
	COALESCE(octet_length(r.root_cutover_uid),0) +
	COALESCE(octet_length(r.conversion_uid),0) +
	9 + CASE WHEN r.owner_generation IS NULL THEN 0 ELSE 8 END`

const scopedBudgetCanonicalCloseBytes = `COALESCE(octet_length(r.issue_uid),0) +
	COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.status_intent_uid),0) +
	COALESCE(octet_length(r.status_clock_bytes),0) +
	COALESCE(octet_length(r.value_bytes),0)`

const scopedBudgetPredicateIncarnationBytes = `COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.subject_uid),0) +
	COALESCE(octet_length(r.predicate_kind),0) +
	COALESCE(octet_length(r.canonical_digest),0) +
	8`

const scopedBudgetMutationExpectationBytes = `COALESCE(octet_length(r.token_digest),0) +
	COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.subject_uid),0) +
	COALESCE(octet_length(r.predicate_kind),0) +
	COALESCE(octet_length(r.predicate_bytes),0) +
	COALESCE(octet_length(r.principal_bytes),0) +
	COALESCE(octet_length(r.route_digest),0) +
	COALESCE(octet_length(r.nonce),0) +
	COALESCE(octet_length(r.expires_at),0) +
	COALESCE(octet_length(r.consumed_operation_uid),0)`

const scopedBudgetProtocolCutoverBytes = `COALESCE(octet_length(r.cutover_uid),0) +
	COALESCE(octet_length(r.request_digest),0) +
	COALESCE(octet_length(r.project_uids_bytes),0) +
	COALESCE(octet_length(r.readiness_bytes),0) +
	COALESCE(octet_length(r.suspended_bindings_bytes),0) +
	COALESCE(octet_length(r.certificate_bytes),0) +
	COALESCE(octet_length(r.state),0) +
	8`

const scopedBudgetConversionBytes = `COALESCE(octet_length(r.conversion_uid),0) +
	COALESCE(octet_length(r.request_digest),0) +
	COALESCE(octet_length(r.request_bytes),0) +
	COALESCE(octet_length(r.old_bindings_bytes),0) +
	COALESCE(octet_length(r.root_cutover_uid),0) +
	COALESCE(octet_length(r.inventory_digest),0) +
	COALESCE(octet_length(r.baseline_digest),0) +
	COALESCE(octet_length(r.phase),0) +
	COALESCE(octet_length(r.result_bytes),0)`

const scopedBudgetSourceOperationBytes = `COALESCE(octet_length(r.operation_uid),0) +
	COALESCE(octet_length(r.digest),0) +
	COALESCE(octet_length(r.source_instance_uid),0) +
	COALESCE(octet_length(r.command_kind),0) +
	COALESCE(octet_length(r.manifest_bytes),0) +
	COALESCE(octet_length(r.principal_bytes),0) +
	COALESCE(octet_length(r.principal_key),0) +
	COALESCE(octet_length(r.state),0) +
	COALESCE(octet_length(r.overlay_state),0) +
	COALESCE(octet_length(r.admission_bytes),0) +
	COALESCE(octet_length(r.terminal_bytes),0) +
	COALESCE(octet_length(r.idempotency_key),0) +
	COALESCE(octet_length(r.request_fingerprint),0) +
	1`

const scopedBudgetPrivateSourceBytes = `COALESCE(octet_length(r.event_uid),0) +
	COALESCE(octet_length(r.content_hash),0) +
	COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.operation_uid),0) +
	COALESCE(octet_length(r.conversion_uid),0) +
	COALESCE(octet_length(r.event_bytes),0) +
	COALESCE(octet_length(r.provenance_bytes),0) +
	CASE WHEN r.ordinal IS NULL THEN 0 ELSE 8 END`

const scopedBudgetSourceDependencyBytes = `COALESCE(octet_length(r.operation_uid),0) +
	COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.issue_uid),0) +
	COALESCE(octet_length(r.establishing_operation_uid),0) +
	COALESCE(octet_length(r.proof_bytes),0) +
	COALESCE(octet_length(r.state),0) +
	COALESCE(octet_length(r.terminal_reason),0)`

const scopedBudgetSourceCustodyBytes = `COALESCE(octet_length(r.operation_uid),0) +
	COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.direction),0) +
	COALESCE(octet_length(r.principal_bytes),0) +
	COALESCE(octet_length(r.manifest_digest),0) +
	COALESCE(octet_length(r.invalidation_state),0) +
	10`

const scopedBudgetSourceCoverageBytes = `COALESCE(octet_length(r.operation_uid),0) +
	COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.cut_digest),0) +
	COALESCE(octet_length(r.outcome),0) +
	COALESCE(octet_length(r.certificate_bytes),0) +
	8`

const scopedBudgetControlCustodyBytes = `COALESCE(octet_length(r.request_uid),0) +
	COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.request_digest),0) +
	COALESCE(octet_length(r.principal_bytes),0) +
	COALESCE(octet_length(r.request_bytes),0) +
	COALESCE(octet_length(r.result_bytes),0) +
	COALESCE(octet_length(r.state),0) +
	COALESCE(octet_length(r.conversion_uid),0) +
	1`

const scopedBudgetConversionObligationBytes = `COALESCE(octet_length(r.conversion_uid),0) +
	COALESCE(octet_length(r.obligation_uid),0) +
	COALESCE(octet_length(r.kind),0) +
	COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.object_uid),0) +
	COALESCE(octet_length(r.digest),0) +
	COALESCE(octet_length(r.principal_key),0) +
	COALESCE(octet_length(r.state),0) +
	16`

const scopedBudgetWorkBytes = `COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.capture_uid),0) +
	COALESCE(octet_length(r.capture_bytes),0) +
	COALESCE(octet_length(r.capture_digest),0) +
	COALESCE(octet_length(r.state),0) +
	8 + CASE WHEN r.captured_revision IS NULL THEN 0 ELSE 8 END`

const scopedBudgetRecordBytes = `COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.collection),0) +
	COALESCE(octet_length(r.record_key),0) +
	COALESCE(octet_length(r.project_uid),0) +
	COALESCE(octet_length(r.subject_uid),0) +
	COALESCE(octet_length(r.peer_project_uid),0) +
	COALESCE(octet_length(r.peer_uid),0) +
	COALESCE(octet_length(r.record_bytes),0) +
	COALESCE(octet_length(r.digest),0) +
	17`

const scopedBudgetBatchBytes = `COALESCE(octet_length(r.view_uid),0) +
	COALESCE(octet_length(r.digest),0) +
	COALESCE(octet_length(r.previous_digest),0) +
	COALESCE(octet_length(r.capture_uid),0) +
	COALESCE(octet_length(r.batch_bytes),0) +
	16`

const scopedBudgetDeliveryRecordBytes = `COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.direction),0) +
	COALESCE(octet_length(r.channel),0) +
	COALESCE(octet_length(r.object_uid),0) +
	COALESCE(octet_length(r.object_digest),0) +
	COALESCE(octet_length(r.offered_digest),0) +
	COALESCE(octet_length(r.header_bytes),0) +
	COALESCE(octet_length(r.envelope_bytes),0) +
	19 + CASE WHEN r.upstream_binding_uid IS NULL THEN 0 ELSE COALESCE(octet_length(r.upstream_binding_uid),0) + COALESCE(octet_length(r.upstream_direction),0) + COALESCE(octet_length(r.upstream_channel),0) + 16 END`

const scopedBudgetDeliveryCursorBytes = `COALESCE(octet_length(r.binding_uid),0) +
	COALESCE(octet_length(r.direction),0) +
	COALESCE(octet_length(r.channel),0) +
	COALESCE(octet_length(r.received_digest),0) +
	COALESCE(octet_length(r.accepted_digest),0) +
	COALESCE(octet_length(r.emitted_digest),0) +
	COALESCE(octet_length(r.acknowledged_digest),0) +
	40`

func ReadScopedSourceBudgetUsageTx(ctx context.Context, tx *sql.Tx) (scopedSourceBudgetUsage, error) {
	usage := newScopedSourceBudgetUsage()
	families := scopedSourceBudgetFamilies()
	for _, family := range families {
		var size int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM("+family.cost+"),0) FROM "+family.table+" r").Scan(&size); err != nil {
			return usage, fmt.Errorf("scoped budget %s: %w", family.kind, err)
		}
		if err := scopedSourceBudgetAdd(usage.FamilyBytes, family.kind, size); err != nil {
			return usage, err
		}
	}
	views := map[string]struct{ binding, route string }{}
	rows, err := tx.QueryContext(ctx, `SELECT view_uid,binding_uid,route_digest FROM federation_scoped_views`)
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var view, binding, route string
		if err := rows.Scan(&view, &binding, &route); err != nil {
			_ = rows.Close()
			return usage, err
		}
		views[view] = struct{ binding, route string }{binding, route}
		usage.PrivateBytes[view] = 0
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT authority_uid,binding_uid,COUNT(*) FROM federation_scoped_views WHERE state IN ('active','prepared') GROUP BY authority_uid,binding_uid`)
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var authority, binding string
		var count int
		if err := rows.Scan(&authority, &binding, &count); err != nil {
			_ = rows.Close()
			return usage, err
		}
		usage.ActiveViews[authority+"/"+binding] = count
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	// Never use the receiving custody binding as an origin principal. Group
	// by small immutable principal bytes in SQL, then merge decoded binding
	// cohorts across encodings/epochs and match each view's binding AND route.
	// LEFT JOIN retains missing owners as an invalid principal, not lost bytes.
	cohorts := `SELECT family,principal_bytes,SUM(cost) FROM (
		SELECT 'source' AS family,r.principal_bytes,` + scopedBudgetSourceOperationBytes + ` AS cost FROM federation_source_operations r
		UNION ALL SELECT 'source',o.principal_bytes,` + scopedBudgetPrivateSourceBytes + ` AS cost FROM federation_private_sources r LEFT JOIN federation_source_operations o ON o.operation_uid=r.operation_uid WHERE r.operation_uid IS NOT NULL
		UNION ALL SELECT 'control',r.principal_bytes,` + scopedBudgetControlCustodyBytes + ` AS cost FROM federation_control_custody r
	) charges GROUP BY family,principal_bytes`
	rows, err = tx.QueryContext(ctx, cohorts)
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var family string
		var raw []byte
		var size int64
		if err := rows.Scan(&family, &raw, &size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		var principal CustodyPrincipal
		var decodeErr error
		switch family {
		case "source":
			decodeErr = scopedJSON(raw, &principal)
		case "control":
			decodeErr = scopedJSON(raw, &principal)
		default:
			decodeErr = errors.New("unknown private scoped budget family")
		}
		if decodeErr != nil {
			_ = rows.Close()
			return usage, decodeErr
		}
		if err := scopedSourceBudgetAdd(usage.BindingBytes, principal.BindingUID, size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		for uid, view := range views {
			if view.binding == principal.BindingUID && view.route == principal.RouteDigest {
				if err := scopedSourceBudgetAdd(usage.PrivateBytes, uid, size); err != nil {
					_ = rows.Close()
					return usage, err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	// Delivery is binding-only, even when its binding also indexes a view.
	rows, err = tx.QueryContext(ctx, "SELECT r.binding_uid,SUM("+scopedBudgetDeliveryRecordBytes+") FROM federation_scoped_delivery r GROUP BY r.binding_uid")
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var binding string
		var size int64
		if err := rows.Scan(&binding, &size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		if err := scopedSourceBudgetAdd(usage.BindingBytes, binding, size); err != nil {
			_ = rows.Close()
			return usage, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	// Work is charged only to its exact owner view, never a route neighbor.
	rows, err = tx.QueryContext(ctx, "SELECT r.view_uid,SUM("+scopedBudgetWorkBytes+") FROM federation_scoped_work r GROUP BY r.view_uid")
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var uid string
		var size int64
		if err := rows.Scan(&uid, &size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		if _, exists := views[uid]; !exists {
			_ = rows.Close()
			return usage, errors.New("scoped budget work lacks owner view")
		}
		if err := scopedSourceBudgetAdd(usage.PrivateBytes, uid, size); err != nil {
			_ = rows.Close()
			return usage, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	// Complete native/import validation already proves these private original
	// bytes equal manifest OriginalBytes. Conversion-only rows do not join.
	rows, err = tx.QueryContext(ctx, `SELECT o.operation_uid,COALESCE(SUM(octet_length(p.event_bytes)),0) FROM federation_source_operations o LEFT JOIN federation_private_sources p ON p.operation_uid=o.operation_uid GROUP BY o.operation_uid`)
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var uid string
		var size int64
		if err := rows.Scan(&uid, &size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		if err := scopedSourceBudgetAdd(usage.OriginalBytes, uid, size); err != nil {
			_ = rows.Close()
			return usage, err
		}
		usage.neighbors[uid] = nil
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT operation_uid,establishing_operation_uid FROM federation_source_dependencies WHERE establishing_operation_uid IS NOT NULL`)
	if err != nil {
		return usage, err
	}
	for rows.Next() {
		var operation, establishing string
		if err := rows.Scan(&operation, &establishing); err != nil {
			_ = rows.Close()
			return usage, err
		}
		if _, exists := usage.OriginalBytes[operation]; !exists {
			_ = rows.Close()
			return usage, errors.New("scoped budget dependency lacks source owner")
		}
		if _, exists := usage.OriginalBytes[establishing]; !exists {
			_ = rows.Close()
			return usage, errors.New("scoped budget dependency lacks establishing owner")
		}
		usage.neighbors[operation] = append(usage.neighbors[operation], establishing)
		usage.neighbors[establishing] = append(usage.neighbors[establishing], operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return usage, err
	}
	if err := rows.Close(); err != nil {
		return usage, err
	}
	return usage, nil
}

func scopedSourceBudgetAdd(totals map[string]int64, key string, size int64) error {
	if size < 0 || totals[key] > math.MaxInt64-size {
		return errors.New("scoped retained-byte accounting overflow")
	}
	totals[key] += size
	return nil
}

func (usage scopedSourceBudgetUsage) validate() error {
	for _, count := range usage.ActiveViews {
		if count > MaxScopedActiveViews {
			return errors.New("scoped active view quota exceeded")
		}
	}
	for _, size := range usage.BindingBytes {
		if size > MaxScopedBindingBytes {
			return errors.New("scoped binding retained-byte quota exceeded; obligations cannot be evicted")
		}
	}
	for _, size := range usage.PrivateBytes {
		if size > MaxScopedRecoveryBytes {
			return errors.New("scoped private recovery quota exceeded")
		}
	}
	var total int64
	for _, size := range usage.FamilyBytes {
		if size < 0 || total > math.MaxInt64-size {
			return errors.New("scoped retained-byte accounting overflow")
		}
		total += size
		if total > MaxScopedDaemonBytes {
			return errors.New("scoped aggregate retained-byte quota exceeded")
		}
	}
	// ONE existing undirected component walk, shared by SQL and import input.
	// Actual membership edges never become event-grouping or eviction hints.
	visited := map[string]bool{}
	for uid := range usage.neighbors {
		if visited[uid] {
			continue
		}
		pending := []string{uid}
		count, bodyBytes := 0, int64(0)
		for len(pending) > 0 {
			current := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if visited[current] {
				continue
			}
			visited[current] = true
			count++
			size := usage.OriginalBytes[current]
			if size < 0 || bodyBytes > math.MaxInt64-size {
				return errors.New("scoped retained-byte accounting overflow")
			}
			bodyBytes += size
			if count > MaxScopedComponentOperations || bodyBytes > MaxScopedComponentOriginalBytes {
				return errors.New("scoped dependency component exceeds retained source bounds")
			}
			pending = append(pending, usage.neighbors[current]...)
		}
	}
	return nil
}

func scopedSourceBudgetFamilies() []struct{ kind, table, cost string } {
	return []struct{ kind, table, cost string }{
		{(*ScopedViewExport)(nil).ImportKind(), "federation_scoped_views", scopedBudgetViewBytes},
		{(*ScopedGenerationExport)(nil).ImportKind(), "federation_scoped_generations", scopedBudgetGenerationBytes},
		{(*ScopedProjectExport)(nil).ImportKind(), "federation_scoped_projects", scopedBudgetProjectBytes},
		{(*ScopedCanonicalCloseExport)(nil).ImportKind(), "federation_canonical_close_state", scopedBudgetCanonicalCloseBytes},
		{(*ScopedPredicateIncarnationExport)(nil).ImportKind(), "federation_predicate_incarnations", scopedBudgetPredicateIncarnationBytes},
		{(*ScopedMutationExpectationExport)(nil).ImportKind(), "federation_mutation_expectations", scopedBudgetMutationExpectationBytes},
		{(*ScopedProtocolCutoverExport)(nil).ImportKind(), "federation_protocol_cutovers", scopedBudgetProtocolCutoverBytes},
		{(*ScopedConversionExport)(nil).ImportKind(), "federation_scoped_conversions", scopedBudgetConversionBytes},
		{(*ScopedSourceOperationExport)(nil).ImportKind(), "federation_source_operations", scopedBudgetSourceOperationBytes},
		{(*ScopedPrivateSourceExport)(nil).ImportKind(), "federation_private_sources", scopedBudgetPrivateSourceBytes},
		{(*ScopedSourceDependencyExport)(nil).ImportKind(), "federation_source_dependencies", scopedBudgetSourceDependencyBytes},
		{(*ScopedSourceCustodyExport)(nil).ImportKind(), "federation_source_custody", scopedBudgetSourceCustodyBytes},
		{(*ScopedSourceCoverageExport)(nil).ImportKind(), "federation_source_coverage", scopedBudgetSourceCoverageBytes},
		{(*ScopedControlCustodyExport)(nil).ImportKind(), "federation_control_custody", scopedBudgetControlCustodyBytes},
		{(*ScopedConversionObligationExport)(nil).ImportKind(), "federation_conversion_obligations", scopedBudgetConversionObligationBytes},
		{(*ScopedWorkExport)(nil).ImportKind(), "federation_scoped_work", scopedBudgetWorkBytes},
		{(*ScopedRecordExport)(nil).ImportKind(), "federation_scoped_records", scopedBudgetRecordBytes},
		{(*ScopedBatchExport)(nil).ImportKind(), "federation_scoped_batches", scopedBudgetBatchBytes},
		{(*ScopedDeliveryRecord)(nil).ImportKind(), "federation_scoped_delivery", scopedBudgetDeliveryRecordBytes},
		{(*ScopedDeliveryCursor)(nil).ImportKind(), "federation_scoped_cursors", scopedBudgetDeliveryCursorBytes},
	}
}
