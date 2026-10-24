package db

import (
	"context"
	"errors"
)

// CreateScopedRelayEnrollmentParams contains trusted native admission inputs.
// Numeric parent/anchor IDs are local to this store and never wire authority.
type CreateScopedRelayEnrollmentParams struct {
	View            ScopedViewDescriptor
	CatalogEntryUID string
	AnchorProjectID int64
	ParentTokenID   int64
	Token           string
	ServeDownstream bool
	RebindParent    bool
}

func (CreateScopedRelayEnrollmentParams) MarshalJSON() ([]byte, error) {
	return nil, errors.New("scoped enrollment parameters are native-only")
}
func (*CreateScopedRelayEnrollmentParams) UnmarshalJSON([]byte) error {
	return errors.New("scoped enrollment parameters are not a wire request")
}

// ScopedRelayEnrollmentStore is optional until real native admission is present.
// Creating a grant prepares private context; it never activates a serving view.
type ScopedRelayEnrollmentStore interface {
	CreateScopedRelayEnrollment(context.Context, CreateScopedRelayEnrollmentParams) (CreatedFederationEnrollment, error)
}
