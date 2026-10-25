package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"reflect"
)

// Only the native retain/preallocation transaction calls this constructor
// after its live authority checks. Descriptor bytes come from that SAME owning
// transaction, never a caller digest or an operation-member-selected sibling.
func scopedSourceCustodyEvidenceTx(ctx context.Context, tx *sql.Tx, view ScopedViewDescriptor, principal CustodyPrincipal) ([]byte, error) {
	var raw []byte
	var digest string
	if err := tx.QueryRowContext(ctx, `SELECT descriptor_bytes,descriptor_digest FROM federation_scoped_generations WHERE view_uid=$1 AND generation=$2`, view.Key.ViewUID, view.Key.Generation).Scan(&raw, &digest); err != nil {
		return nil, err
	}
	var actual ScopedViewDescriptor
	if err := DecodeScopedRecord(raw, &actual); err != nil {
		return nil, err
	}
	if ScopedOfferedDigest(raw) != digest || !reflect.DeepEqual(actual, view) || principal.AuthorityUID != actual.AuthorityUID || principal.RouteDigest != actual.RouteDigest || !scopedDescriptorHasHop(actual, principal.BindingUID, principal.Epoch, "") {
		return nil, ErrScopedSourceConflict
	}
	evidence := ScopedSourceCustodyEvidence{Version: 1, Principal: principal, View: actual.Key, DescriptorDigest: digest}
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(evidence)
}

// ValidateScopedSourceCustodyViewTx checks exact durable view correspondence
// after the caller rederives live native authority. Legacy nil evidence remains
// usable by an explicit authenticated retry; it supplies no background view.
func ValidateScopedSourceCustodyViewTx(ctx context.Context, tx *sql.Tx, evidence *ScopedSourceCustodyEvidence, view ScopedViewDescriptor) error {
	if evidence == nil {
		return nil
	}
	if evidence.View != view.Key {
		return ErrScopedSourceConflict
	}
	raw, err := scopedSourceCustodyEvidenceTx(ctx, tx, view, evidence.Principal)
	if err != nil {
		return err
	}
	_, actual, err := DecodeScopedSourceCustodyPrincipal(raw)
	if err != nil || actual == nil || *actual != *evidence {
		return ErrScopedSourceConflict
	}
	return nil
}
