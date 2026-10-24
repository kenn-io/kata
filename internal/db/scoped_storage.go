package db

import "context"

// These contracts are required together before enabling scoped relay support.
// Every method must enforce context authority and native transaction fences.
// Native stores deliberately do not assert implementation until admission exists.
type ScopedViewStore interface {
	CaptureScopedView(context.Context, ScopedViewKey) (CanonicalViewState, error)
	CommitScopedBatch(context.Context, ScopedBatch, string) error
	PrepareScopedCheckpoint(context.Context, ScopedCheckpoint) error
	ActivateScopedCheckpoint(context.Context, ScopedViewKey, string) error
	ApplyScopedBatch(context.Context, ScopedBatch) error
	FenceScopedView(context.Context, ScopedViewKey, string) error
	ReadScopedView(context.Context, ScopedReadQuery) (ScopedReadResult, error)
}
type ScopedSourceStore interface {
	SubmitScopedCommand(context.Context, ScopedCommand) (SourceStatus, error)
	RetainSourceOperation(context.Context, CustodyPrincipal, SourceSubmission) (SourceStatus, error)
	PrepareMutation(context.Context, ScopedCommand) (MutationExpectation, error)
	AdjudicateSourceOperation(context.Context, string, string) (SourceResult, error)
	CancelSourceOperation(context.Context, string, string) (SourceResult, error)
	ApplySourceSettlement(context.Context, SourceResult, *SourceCoverage) error
	RecoverSourceOutcome(context.Context, string, string) (SourceStatus, error)
}
type ScopedControlStore interface {
	RetainScopedControl(context.Context, CustodyPrincipal, ScopedControlRequest) error
	CompleteScopedControl(context.Context, ScopedControlResult) error
}
type ScopedConversionStore interface {
	PrepareScopedConversion(context.Context, ScopedConversionRequest) (ScopedConversionState, error)
	FenceScopedConversion(context.Context, string, string) (ScopedConversionState, error)
	ScopedConversionObligations(context.Context, string) ([]ScopedObligation, error)
	ActivateScopedConversion(context.Context, string, string, ScopedCheckpoint) (ScopedConversionState, error)
	AbortScopedConversion(context.Context, string, string) error
}

// RootSigners is transient owner-local key material, keyed by project UID.
type RootSigners map[string]RootAttributionSigner
