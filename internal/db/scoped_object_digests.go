package db

import (
	"errors"
	"go.kenn.io/kata/internal/uid"
)

// Object commitments are distinct from exact Body/Offered/Header digests.
// They validate typed shape, without asserting authorization or crypto success.
func ScopedSourceResultObjectDigest(r SourceResult) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	raw, err := ScopedSourceResultSigningBytes(r)
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedSourceCoverageObjectDigest(c SourceCoverage) (string, error) {
	if !uid.Valid(c.OperationUID) || !scopedDigest(c.OperationDigest) {
		return "", errors.New("invalid scoped coverage identity")
	}
	if err := c.View.Validate(); err != nil {
		return "", err
	}
	if err := c.Cut.Validate(); err != nil {
		return "", err
	}
	switch c.Outcome {
	case "represented", "unchanged", "outside_scope":
	default:
		return "", errors.New("unknown scoped coverage outcome")
	}
	raw, err := ScopedSourceCoverageSigningBytes(c)
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedControlRequestObjectDigest(r ScopedControlRequest) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	raw, err := scopedTranscript(ScopedResultDomain, struct {
		Kind    string               `json:"kind"`
		Request ScopedControlRequest `json:"request"`
	}{"control_request", r})
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedControlResultObjectDigest(r ScopedControlResult) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	raw, err := scopedTranscript(ScopedResultDomain, struct {
		Kind   string              `json:"kind"`
		Result ScopedControlResult `json:"result"`
	}{"control_result", r})
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedConversionRequestObjectDigest(r ScopedConversionRequest) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	raw, err := scopedTranscript(ScopedCutoverDomain, struct {
		Kind    string                  `json:"kind"`
		Request ScopedConversionRequest `json:"request"`
	}{"conversion_request", r})
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedConversionStateObjectDigest(s ScopedConversionState) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	raw, err := scopedTranscript(ScopedCutoverDomain, struct {
		Kind  string                `json:"kind"`
		State ScopedConversionState `json:"state"`
	}{"conversion_state", s})
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedCutoverCertificateObjectDigest(c ScopedProtocolCutoverCertificate) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	raw, err := ScopedCutoverCertificateSigningBytes(c)
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
