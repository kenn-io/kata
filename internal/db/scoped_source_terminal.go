package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// Root-private terminal storage is distinct from the public certificate. These
// fields never enter SourceResult or a receiving hop's terminal wire format.
type scopedImportTerminal struct {
	Version   int                    `json:"terminal_version"`
	Result    *SourceResult          `json:"result"`
	Execution *scopedImportExecution `json:"execution"`
}

type scopedImportExecution struct {
	Version        int                         `json:"version"`
	ProjectUID     string                      `json:"project_uid"`
	Salt           []byte                      `json:"salt"`
	Outputs        []scopedImportNativeOutput  `json:"outputs"`
	MappingEffects []scopedImportMappingEffect `json:"mapping_effects"`
}

type scopedImportNativeOutput struct {
	EventBytes []byte             `json:"event_bytes"`
	Receipt    AttributionReceipt `json:"receipt"`
}

type scopedImportMappingEffect struct {
	Before *ImportMappingExport `json:"before"`
	After  *ImportMappingExport `json:"after"`
}

// DecodeScopedSourceTerminalResult extracts only the public certificate from
// either legacy/public terminal bytes or the closed root-private import frame.
// It grants no caller authority and verifies no signature, execution, receipt
// or coverage. Native admission and the complete owner graph retain those
// checks; callers must still verify the result against its operation and pins.
func DecodeScopedSourceTerminalResult(raw []byte) (SourceResult, error) {
	result, _, err := decodeScopedSourceTerminal(raw)
	return result, err
}

// Both public extraction and retained-output verification use this one closed
// decoder. The execution value stays private until its owning root, operation,
// native originals and attribution have also been verified.
func decodeScopedSourceTerminal(raw []byte) (SourceResult, *scopedImportExecution, error) {
	if len(raw) == 0 || len(raw) > MaxScopedEnvelopeBytes {
		return SourceResult{}, nil, errors.New("missing or oversized source terminal")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return SourceResult{}, nil, err
	}
	if _, private := fields["terminal_version"]; !private {
		var result SourceResult
		if err := DecodeScopedRecord(raw, &result); err != nil {
			return SourceResult{}, nil, err
		}
		return result, nil, nil
	}
	var terminal scopedImportTerminal
	if err := DecodeScopedRecord(raw, &terminal); err != nil {
		return SourceResult{}, nil, err
	}
	if terminal.Version != 1 || terminal.Result == nil || terminal.Execution == nil {
		return SourceResult{}, nil, errors.New("invalid root import terminal frame")
	}
	result, execution := terminal.Result, terminal.Execution
	if err := result.Validate(); err != nil {
		return SourceResult{}, nil, err
	}
	if result.ImportOutcome == nil || execution.Version != 1 || !uid.Valid(execution.ProjectUID) || len(execution.Salt) != 32 {
		return SourceResult{}, nil, errors.New("invalid root import execution seal")
	}
	var executionFields map[string]jsontext.Value
	if err := json.Unmarshal(fields["execution"], &executionFields); err != nil {
		return SourceResult{}, nil, err
	}
	for _, name := range []string{"version", "project_uid", "salt", "outputs", "mapping_effects"} {
		value, present := executionFields[name]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return SourceResult{}, nil, errors.New("incomplete root import execution seal")
		}
	}
	if result.State != "accepted" && (len(execution.Outputs) != 0 || len(execution.MappingEffects) != 0) {
		return SourceResult{}, nil, errors.New("unsuccessful import terminal contains execution effects")
	}
	for _, output := range execution.Outputs {
		if len(output.EventBytes) == 0 {
			return SourceResult{}, nil, errors.New("incomplete root-native import output")
		}
	}
	for _, effect := range execution.MappingEffects {
		if effect.Before == nil && effect.After == nil {
			return SourceResult{}, nil, errors.New("empty root import mapping effect")
		}
	}
	commitment, err := scopedImportExecutionCommitment(*result, *execution)
	if err != nil {
		return SourceResult{}, nil, err
	}
	if commitment != result.ImportOutcome.Commitment {
		return SourceResult{}, nil, errors.New("root import execution commitment mismatch")
	}
	return *result, execution, nil
}

func scopedImportExecutionCommitment(result SourceResult, execution scopedImportExecution) (string, error) {
	type executionRecord struct {
		OperationUID    string                `json:"operation_uid"`
		OperationDigest string                `json:"operation_digest"`
		State           string                `json:"state"`
		Execution       scopedImportExecution `json:"execution"`
	}
	record := executionRecord{result.OperationUID, result.OperationDigest, result.State, execution}
	// This unsigned private commitment is covered by the public result's
	// existing signature. It adds no signature domain or portable credential.
	raw, err := json.Marshal(struct {
		Domain string          `json:"domain"`
		Record executionRecord `json:"record"`
	}{"kata.scoped.import.execution.v1", record})
	if err != nil {
		return "", err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return "", err
	}
	return ScopedOfferedDigest(canonical), nil
}
