package db

import "bytes"

// NewScopedReferenceAliasResolver captures identity DATA independently of the
// authority in endpoints. Its caller supplies native same-transaction rows or
// identities from the existing complete validated retained creator relation.
// It grants no membership/creation/liveness authority and performs no reads,
// payload parsing, UID/suffix inference, normalization or alias rewriting.
func NewScopedReferenceAliasResolver(identities []ScopedIssueState, endpoints map[string]ScopedEndpoint) func(string, string) (ScopedEndpoint, error) {
	allowed := make(map[string]ScopedEndpoint, len(endpoints))
	invalid := false
	for key, endpoint := range endpoints {
		if endpoint.Validate() != nil || endpoint.IssueUID != key {
			invalid = true
		}
		allowed[key] = endpoint
	}
	type aliasKey struct{ project, short string }
	aliases := map[aliasKey]ScopedEndpoint{}
	ambiguous := map[aliasKey]bool{}
	byUID := map[string]aliasKey{}
	for _, identity := range identities {
		endpoint, eligible := allowed[identity.UID]
		if !eligible {
			continue
		}
		if identity.ProjectUID != endpoint.ProjectUID || identity.UID != endpoint.IssueUID || identity.ShortID == "" {
			invalid = true
			continue
		}
		key := aliasKey{identity.ProjectUID, identity.ShortID}
		if prior, exists := byUID[identity.UID]; exists && prior != key {
			invalid = true
		}
		byUID[identity.UID] = key
		if prior, exists := aliases[key]; exists && prior != endpoint {
			ambiguous[key] = true
		}
		aliases[key] = endpoint
	}
	return func(projectUID, shortID string) (ScopedEndpoint, error) {
		if invalid {
			return ScopedEndpoint{}, ErrScopedSourceConflict
		}
		key := aliasKey{projectUID, shortID}
		if ambiguous[key] {
			return ScopedEndpoint{}, ErrScopedSourceConflict
		}
		endpoint, found := aliases[key]
		if !found {
			return ScopedEndpoint{}, ErrNotFound
		}
		return endpoint, nil
	}
}

// ScopedValidatedRetainedReferenceIdentities projects alias DATA from the ONE
// complete retained owner graph already accepted by ValidateImportRecords. It
// performs no IO, owner validation, fold or admission. Callers must not pass a
// partial export, unvalidated originals or a current-row authority substitute.
func ScopedValidatedRetainedReferenceIdentities(records []ImportRecord, endpoints map[string]ScopedEndpoint) ([]ScopedIssueState, error) {
	projects := map[int64]string{}
	canonical := map[string]bool{}
	operations := map[string]*ScopedSourceOperationExport{}
	complete := map[string]bool{}
	dependencies := map[string][]*ScopedSourceDependencyExport{}
	var identities []ScopedIssueState
	for _, record := range records {
		switch row := record.(type) {
		case *ProjectExport:
			projects[row.ID] = row.UID
		case *ScopedSourceOperationExport:
			operations[row.OperationUID] = row
		case *ScopedSourceCustodyExport:
			if row.OwnsComplete && row.InvalidationState == "none" {
				complete[row.OperationUID] = true
			}
		case *ScopedSourceDependencyExport:
			dependencies[row.OperationUID] = append(dependencies[row.OperationUID], row)
		}
	}
	for _, record := range records {
		row, ok := record.(*IssueExport)
		if !ok {
			continue
		}
		if _, eligible := endpoints[row.UID]; !eligible {
			continue
		}
		identities = append(identities, ScopedIssueState{UID: row.UID, ProjectUID: projects[row.ProjectID], ShortID: row.ShortID})
		canonical[row.UID] = true
	}
	submissions := map[string]SourceSubmission{}
	for _, record := range records {
		row, ok := record.(*ScopedPrivateSourceExport)
		if !ok {
			continue
		}
		event, err := DecodeRelaySourceEvent(row.EventBytes)
		if err != nil {
			return nil, err
		}
		if event.IssueUID == nil || canonical[*event.IssueUID] {
			continue
		}
		endpoint, eligible := endpoints[*event.IssueUID]
		if !eligible || event.Type != "issue.created" {
			continue
		}
		if endpoint.ProjectUID != event.ProjectUID || row.OperationUID == nil || row.ConversionUID != nil {
			return nil, ErrScopedSourcePending
		}
		owner := operations[*row.OperationUID]
		if owner == nil || (owner.State != "staged" && owner.State != "accepted") || !complete[owner.OperationUID] {
			return nil, ErrScopedSourcePending
		}
		input, decoded := submissions[owner.OperationUID]
		if !decoded {
			input, err = DecodeScopedSourceSubmission(owner.ManifestBytes)
			if err != nil {
				return nil, err
			}
			submissions[owner.OperationUID] = input
		}
		if row.Ordinal == nil || *row.Ordinal < 0 || *row.Ordinal >= int64(len(input.Operation.Members)) {
			return nil, ErrScopedSourceConflict
		}
		ordinal := int(*row.Ordinal)
		member := input.Operation.Members[ordinal]
		if input.Operation.UID != owner.OperationUID || input.Operation.Digest != owner.Digest ||
			owner.RequestFingerprint != ScopedOfferedDigest(owner.ManifestBytes) || member.EventUID != row.EventUID ||
			member.ContentHash != row.ContentHash || member.ProjectUID != row.ProjectUID ||
			!bytes.Equal(input.OriginalBytes[ordinal], row.EventBytes) {
			return nil, ErrScopedSourceConflict
		}
		established := false
		for _, dependency := range dependencies[owner.OperationUID] {
			if dependency.ProjectUID == endpoint.ProjectUID && dependency.IssueUID == endpoint.IssueUID &&
				dependency.EstablishingOperationUID != nil && *dependency.EstablishingOperationUID == owner.OperationUID &&
				(dependency.State == "pending" || dependency.State == "satisfied") {
				var proof ScopedMembershipProof
				if err := DecodeScopedRecord(dependency.ProofBytes, &proof); err != nil {
					return nil, err
				}
				established = proof.Kind == "component_creation" && proof.Endpoint == endpoint &&
					proof.CreatingOperationUID == owner.OperationUID && scopedSourceCreation(input, proof)
				if established {
					break
				}
			}
		}
		if !established {
			return nil, ErrScopedSourcePending
		}
		var primary NativeIssueCreatedPayload
		if err := DecodeScopedRecord(event.Payload, &primary); err != nil {
			return nil, err
		}
		if primary.UID != endpoint.IssueUID || primary.ShortID == "" {
			return nil, ErrScopedSourceConflict
		}
		identities = append(identities, ScopedIssueState{UID: primary.UID, ProjectUID: event.ProjectUID, ShortID: primary.ShortID})
	}
	// Root-native V3 births are distinct from producer members. The SAME typed
	// accessor verifies private execution ownership/result/commitment/receipts.
	var ownerInstance string
	var pins []RootKeyPin
	for _, record := range records {
		switch row := record.(type) {
		case *MetaKV:
			if row.Key == "instance_uid" {
				ownerInstance = row.Value
			}
		case *RootKeyPin:
			pins = append(pins, *row)
		}
	}
	sourceRows := func(yield func(ImportRecord, error) bool) {
		for _, record := range records {
			if _, ok := record.(*ScopedSourceOperationExport); ok && !yield(record, nil) {
				return
			}
		}
	}
	if err := VisitScopedImportTerminalOutputs(sourceRows, pins, ownerInstance, func(output ScopedImportRetainedOutput) error {
		event := output.Event
		if event.Type != "issue.created" || event.IssueUID == nil || canonical[*event.IssueUID] {
			return nil
		}
		endpoint, eligible := endpoints[*event.IssueUID]
		if !eligible {
			return nil
		}
		var primary NativeIssueCreatedPayload
		if err := DecodeScopedRecord(event.Payload, &primary); err != nil {
			return err
		}
		if primary.UID != endpoint.IssueUID || event.ProjectUID != endpoint.ProjectUID || primary.ShortID == "" {
			return ErrScopedSourceConflict
		}
		identities = append(identities, ScopedIssueState{UID: primary.UID, ProjectUID: event.ProjectUID, ShortID: primary.ShortID})
		return nil
	}); err != nil {
		return nil, err
	}
	return identities, nil
}
