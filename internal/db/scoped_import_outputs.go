package db

import (
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// ScopedImportRetainedOutput is a verified, root-private native output, not a
// producer member or serving credential. Native clock/identity readers consume
// this same typed view after the owning terminal has been verified below.
type ScopedImportRetainedOutput struct {
	EventBytes []byte
	Event      RemoteEvent
	Receipt    AttributionReceipt
}

// VerifyScopedImportTerminalOutputs validates the exact retained operation and
// its signed terminal before returning any root-private output witness. A public
// outcome certificate carries no output custody, including at a receiving hop.
// This verifies durable evidence, not live authority to execute or serve it.
func VerifyScopedImportTerminalOutputs(row ScopedSourceOperationExport, pins []RootKeyPin, ownerInstanceUID string) ([]ScopedImportRetainedOutput, error) {
	if err := ValidateScopedImportRecord(&row); err != nil {
		return nil, err
	}
	submission, err := DecodeScopedSourceSubmission(row.ManifestBytes)
	if err != nil {
		return nil, err
	}
	op := submission.Operation
	result, execution, err := decodeScopedSourceTerminal(row.TerminalBytes)
	if err != nil {
		return nil, err
	}
	if err := VerifyScopedSourceResult(result, op, pins); err != nil {
		return nil, err
	}
	if op.Version == 3 && !uid.Valid(ownerInstanceUID) {
		return nil, errors.New("import output custody requires the actual owning instance")
	}
	if execution == nil {
		if op.Version == 3 && ownerInstanceUID == op.Principal.AuthorityUID {
			return nil, errors.New("root import terminal is missing its private execution custody")
		}
		return nil, nil
	}
	return verifyScopedImportExecutionOutputs(op, *execution, pins, ownerInstanceUID)
}

func verifyScopedImportExecutionOutputs(op SourceOperation, execution scopedImportExecution, pins []RootKeyPin, ownerInstanceUID string) ([]ScopedImportRetainedOutput, error) {
	if op.Version != 3 || ownerInstanceUID != op.Principal.AuthorityUID || execution.ProjectUID != op.ImportIntent.ProjectUID {
		return nil, errors.New("private import execution differs from owning root or intent project")
	}
	trusted, err := scopedTrustedPins(pins)
	if err != nil {
		return nil, err
	}
	outputs := make([]ScopedImportRetainedOutput, 0, len(execution.Outputs))
	seen := map[string]bool{}
	for _, output := range execution.Outputs {
		event, err := DecodeRelaySourceEvent(output.EventBytes)
		if err != nil {
			return nil, err
		}
		if !uid.Valid(event.EventUID) || event.ProjectUID != execution.ProjectUID || event.OriginInstanceUID != ownerInstanceUID || event.HLCPhysicalMS < 0 || event.HLCCounter < 0 || seen[event.EventUID] {
			return nil, errors.New("invalid or duplicate root-native import output identity")
		}
		seen[event.EventUID] = true
		if _, known := ScopedEventClass(event.Type); !known {
			return nil, errors.New("unknown root-native import output family")
		}
		if err := ValidateFederationEventEntries(event); err != nil {
			return nil, err
		}
		handle, _, err := EventCreationProvenance(event)
		if err != nil {
			return nil, err
		}
		receipt := output.Receipt
		if receipt.ProjectUID != event.ProjectUID || receipt.EventUID != event.EventUID || receipt.ContentHash != event.ContentHash || receipt.SourceActor != event.Actor || receipt.Teammate != handle || receipt.AuthorityUID != ownerInstanceUID || receipt.IngressInstanceUID != ownerInstanceUID {
			return nil, errors.New("root import output differs from its native attribution")
		}
		pin, err := trusted.get(receipt.ProjectUID, receipt.KeyID, ownerInstanceUID)
		if err != nil {
			return nil, err
		}
		if err := VerifyRootReceipt(pin, receipt); err != nil {
			return nil, err
		}
		outputs = append(outputs, ScopedImportRetainedOutput{EventBytes: output.EventBytes, Event: event, Receipt: receipt})
	}
	return outputs, nil
}
