package daemon

import (
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func federationReadOnlyError(err error) error {
	if errors.Is(err, db.ErrFederatedMoveUnsupported) {
		return api.NewError(409, "federated_move_unsupported",
			"cross-project moves involving a federated project are unsupported",
			"both projects must be unfederated; disabling sync retains the federation binding", nil)
	}
	if errors.Is(err, db.ErrFederatedReadOnly) {
		if errors.Is(err, db.ErrRecurringHubRequired) {
			return api.NewError(409, "federated_read_only", db.ErrRecurringHubRequired.Error(),
				"Run the same command against the project's owning hub, for example with --daemon <hub-name>.", nil)
		}
		return api.NewError(409, "federated_read_only", err.Error(), "", nil)
	}
	return nil
}
