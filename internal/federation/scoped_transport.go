package federation

import (
	"context"
	"go.kenn.io/kata/internal/db"
)

// ScopedTransport is private until the complete scoped capability is enabled.
// Coverage certificates travel only in authenticated custodian delivery records.
// PrepareMutation accepts stable wire identities; native ScopedCommand must
// never cross this boundary.
type ScopedTransport interface {
	RequestView(context.Context, db.ScopedViewDescriptor) (db.ScopedCheckpoint, error)
	Offer(context.Context, string, db.ScopedCut) ([]db.ScopedBatch, error)
	Ack(context.Context, string, db.ScopedCut) error
	Submit(context.Context, db.SourceSubmission) (db.SourceStatus, error)
	Outcome(context.Context, string, string) (db.SourceStatus, error)
	Cancel(context.Context, string, string) (db.SourceResult, error)
	PrepareMutation(context.Context, db.ScopedMutationPreparation) (db.MutationExpectation, error)
	Control(context.Context, db.ScopedControlRequest) (db.ScopedControlResult, error)
	Convert(context.Context, db.ScopedConversionRequest) (db.ScopedConversionState, error)
}
