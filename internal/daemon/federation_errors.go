package daemon

import (
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func federationReadOnlyError(err error) error {
	if errors.Is(err, db.ErrFederatedMoveUnsupported) {
		return api.NewError(409, "federated_move_unsupported",
			"cross-project moves involving a federated project are unsupported unless both projects use the same active hub",
			"create the issue in the target project and link it to the original issue", nil)
	}
	if errors.Is(err, db.ErrFederatedReadOnly) {
		return api.NewError(409, "federated_read_only", err.Error(), "", nil)
	}
	return nil
}
