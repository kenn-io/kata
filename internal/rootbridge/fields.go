package rootbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/connector"
)

type invalidExternalFieldReadError struct {
	cause error
}

func (e *invalidExternalFieldReadError) Error() string { return e.cause.Error() }
func (e *invalidExternalFieldReadError) Unwrap() error { return e.cause }

func isInvalidExternalFieldRead(err error) bool {
	var invalid *invalidExternalFieldReadError
	return errors.As(err, &invalid)
}

// MergeAction describes the reconciliation work needed for one mapped field.
type MergeAction string

const (
	// MergeAccept records a shared value without writing either side.
	MergeAccept MergeAction = "accept"
	// MergeWriteKata applies the external value to Kata.
	MergeWriteKata MergeAction = "write_kata"
	// MergeWriteExternal applies the Kata value to the external root.
	MergeWriteExternal MergeAction = "write_external"
	// MergeConflict records divergent changes for explicit resolution.
	MergeConflict MergeAction = "conflict"
)

// MergeDecision retains the canonical values used to make a field decision.
// A conflict deliberately leaves Baseline unchanged while retaining both
// candidates for explicit resolution.
type MergeDecision struct {
	Action   MergeAction
	Baseline connector.FieldValue
	Kata     connector.FieldValue
	External connector.FieldValue
}

// DecideInitialFieldMerge reconciles a field before a stored baseline exists.
func DecideInitialFieldMerge(kata, external connector.FieldValue) MergeDecision {
	kata = canonicalForMerge(kata)
	external = canonicalForMerge(external)
	if fieldValuesEqual(kata, external) {
		return MergeDecision{Action: MergeAccept, Baseline: kata, Kata: kata, External: external}
	}
	if isNull(kata) {
		return MergeDecision{Action: MergeWriteKata, Baseline: external, Kata: kata, External: external}
	}
	if isNull(external) {
		return MergeDecision{Action: MergeWriteExternal, Baseline: kata, Kata: kata, External: external}
	}
	return MergeDecision{Action: MergeConflict, Baseline: nullFieldValue(), Kata: kata, External: external}
}

// DecideFieldMerge compares the two current values with their stored baseline.
func DecideFieldMerge(baseline, kata, external connector.FieldValue) MergeDecision {
	baseline = canonicalForMerge(baseline)
	kata = canonicalForMerge(kata)
	external = canonicalForMerge(external)
	if fieldValuesEqual(kata, external) {
		return MergeDecision{Action: MergeAccept, Baseline: kata, Kata: kata, External: external}
	}
	if fieldValuesEqual(kata, baseline) {
		return MergeDecision{Action: MergeWriteKata, Baseline: external, Kata: kata, External: external}
	}
	if fieldValuesEqual(external, baseline) {
		return MergeDecision{Action: MergeWriteExternal, Baseline: kata, Kata: kata, External: external}
	}
	return MergeDecision{Action: MergeConflict, Baseline: baseline, Kata: kata, External: external}
}

func canonicalForMerge(value connector.FieldValue) connector.FieldValue {
	canonical, err := canonicalFieldValue(value)
	if err != nil {
		return value
	}
	return canonical
}

func fieldValuesEqual(left, right connector.FieldValue) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

func isNull(value connector.FieldValue) bool {
	return value.Kind == "null"
}

func validateLiveFieldDescriptor(
	codec FieldCodec,
	mapping db.ExternalFieldMapping,
	fields []connector.FieldDescriptor,
) (connector.FieldDescriptor, error) {
	matches := make([]connector.FieldDescriptor, 0, 1)
	for _, field := range fields {
		if field.ID == mapping.ExternalFieldID {
			matches = append(matches, field)
		}
	}
	if len(matches) != 1 {
		return connector.FieldDescriptor{}, fmt.Errorf("mapped external field identity changed")
	}
	descriptor := matches[0]
	if err := codec.ValidateExternalDescriptor(descriptor); err != nil {
		return connector.FieldDescriptor{}, err
	}
	liveKinds := append([]string(nil), descriptor.AcceptedKinds...)
	sort.Strings(liveKinds)
	mappedKinds := append([]string(nil), mapping.AcceptedKinds...)
	sort.Strings(mappedKinds)
	if descriptor.SchemaRevision != mapping.SchemaRevision || descriptor.Nullable != mapping.Nullable ||
		descriptor.Writable != mapping.Writable || !stringSlicesEqual(liveKinds, mappedKinds) {
		return connector.FieldDescriptor{}, fmt.Errorf("mapped external field schema changed")
	}
	return descriptor, nil
}

func canonicalMappedFieldValue(
	value connector.FieldValue,
	mapping db.ExternalFieldMapping,
) (connector.FieldValue, error) {
	if value.Kind == "" {
		return connector.FieldValue{}, fmt.Errorf("mapped external field kind is required")
	}
	canonical, err := canonicalFieldValue(value)
	if err != nil {
		return connector.FieldValue{}, err
	}
	if canonical.Kind == fieldKindNull {
		if !mapping.Nullable {
			return connector.FieldValue{}, fmt.Errorf("mapped external field is not nullable")
		}
		return canonical, nil
	}
	if slices.Contains(mapping.AcceptedKinds, canonical.Kind) {
		return canonical, nil
	}
	return connector.FieldValue{}, fmt.Errorf("mapped external field does not accept %q", canonical.Kind)
}

func decodeCanonicalStoredFieldValue(
	raw json.RawMessage,
	mapping db.ExternalFieldMapping,
) (connector.FieldValue, bool, error) {
	if len(raw) == 0 {
		return connector.FieldValue{}, false, nil
	}
	var value connector.FieldValue
	if err := json.Unmarshal(raw, &value); err != nil {
		return connector.FieldValue{}, false, err
	}
	canonical, err := canonicalMappedFieldValue(value, mapping)
	if err != nil {
		return connector.FieldValue{}, false, err
	}
	return canonical, true, nil
}

func marshalCanonicalFieldValue(value connector.FieldValue) (json.RawMessage, error) {
	canonical, err := canonicalFieldValue(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
