package daemon

import (
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func federationReadOnlyError(err error) error {
	if errors.Is(err, db.ErrFederatedReadOnly) {
		return api.NewError(409, "federated_read_only", err.Error(), "", nil)
	}
	return nil
}
