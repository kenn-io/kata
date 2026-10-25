package db

import (
	"errors"
	"iter"
)

// VisitScopedImportTerminalOutputs consumes the existing typed operation reader
// and the ONE closed terminal verifier. Allocation uses this below adjudication;
// it does not materialize checkpoints, validate a second owner graph, mint an
// original, or infer authority from an operation state. The caller supplies its
// actual retained owner and historical pins from the same owning transaction.
// A callback must not query through an iterator's still-open SQL cursor.
func VisitScopedImportTerminalOutputs(rows iter.Seq2[ImportRecord, error], pins []RootKeyPin, ownerInstanceUID string, visit func(ScopedImportRetainedOutput) error) error {
	if rows == nil || visit == nil {
		return errors.New("missing retained import operation reader or consumer")
	}
	for record, err := range rows {
		if err != nil {
			return err
		}
		row, ok := record.(*ScopedSourceOperationExport)
		if !ok || row == nil {
			return errors.New("import terminal reader returned a different typed family")
		}
		if err := ValidateScopedImportRecord(row); err != nil {
			return err
		}
		if row.TerminalBytes == nil {
			continue // A retained input is not a root-produced original.
		}
		submission, err := DecodeScopedSourceSubmission(row.ManifestBytes)
		if err != nil {
			return err
		}
		if submission.Operation.Version != 3 {
			continue // V1/V2 originals remain in the existing private-source reader.
		}
		outputs, err := VerifyScopedImportTerminalOutputs(*row, pins, ownerInstanceUID)
		if err != nil {
			return err
		}
		for _, output := range outputs {
			if err := visit(output); err != nil {
				return err
			}
		}
	}
	return nil
}
