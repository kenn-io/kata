package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// ScopedSourceCustodyEvidence is private durable admission evidence, not caller
// authority or a claim-holder credential. Its view names the actual generation
// admitted by the native retain transaction, including its exact stored bytes.
type ScopedSourceCustodyEvidence struct {
	Version          int              `json:"version"`
	Principal        CustodyPrincipal `json:"principal"`
	View             ScopedViewKey    `json:"view"`
	DescriptorDigest string           `json:"descriptor_digest"`
}

func (e ScopedSourceCustodyEvidence) Validate() error {
	if e.Version != 1 || e.Principal.Validate() != nil || e.View.Validate() != nil || !scopedDigest(e.DescriptorDigest) {
		return errors.New("invalid source custody view evidence")
	}
	return nil
}

// DecodeScopedSourceCustodyPrincipal is the ONE reader for legacy plain tuples
// and the closed private view-bound frame. A nil evidence value proves no view;
// it must not trigger background sibling selection or a legacy-row backfill.
func DecodeScopedSourceCustodyPrincipal(raw []byte) (CustodyPrincipal, *ScopedSourceCustodyEvidence, error) {
	if len(raw) == 0 || len(raw) > MaxScopedEnvelopeBytes {
		return CustodyPrincipal{}, nil, errors.New("missing or oversized source custody evidence")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return CustodyPrincipal{}, nil, err
	}
	if _, framed := fields["version"]; !framed {
		var principal CustodyPrincipal
		if err := DecodeScopedRecord(raw, &principal); err != nil {
			return CustodyPrincipal{}, nil, err
		}
		if err := principal.Validate(); err != nil {
			return CustodyPrincipal{}, nil, err
		}
		return principal, nil, nil
	}
	for _, name := range []string{"version", "principal", "view", "descriptor_digest"} {
		value, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return CustodyPrincipal{}, nil, errors.New("incomplete source custody view evidence")
		}
	}
	var evidence ScopedSourceCustodyEvidence
	if err := DecodeScopedRecord(raw, &evidence); err != nil {
		return CustodyPrincipal{}, nil, err
	}
	if err := evidence.Validate(); err != nil {
		return CustodyPrincipal{}, nil, err
	}
	return evidence.Principal, &evidence, nil
}
