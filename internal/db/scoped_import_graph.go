package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/kata/internal/uid"
	"reflect"
	"slices"
	"strings"
	"time"
)

type scopedPredicateKey struct{ project, subject, kind string }
type scopedDependencyKey struct{ operation, project, issue string }
type scopedImportGraph struct {
	projects      map[string]*ProjectExport
	projectIDs    map[int64]string
	issues        map[string]*IssueExport
	pins          []RootKeyPin
	views         map[string]*ScopedViewExport
	generations   map[ScopedViewKey]*ScopedGenerationExport
	descriptors   map[ScopedViewKey]ScopedViewDescriptor
	ownership     map[string]*ScopedProjectExport
	cuts          map[ScopedViewKey]map[string]ScopedCut
	cutovers      map[string]*ScopedProtocolCutoverExport
	conversions   map[string]*ScopedConversionExport
	operations    map[string]*ScopedSourceOperationExport
	submissions   map[string]SourceSubmission
	originals     map[string]*ScopedPrivateSourceExport
	dependencies  map[scopedDependencyKey]*ScopedSourceDependencyExport
	expectations  map[string]*ScopedMutationExpectationExport
	incarnations  map[scopedPredicateKey]*ScopedPredicateIncarnationExport
	deliveries    map[ScopedDeliveryRef]*ScopedDeliveryRecord
	cursors       map[ScopedDeliveryStream]*ScopedDeliveryCursor
	controls      map[string]*ScopedControlCustodyExport
	principalKeys map[string]CustodyPrincipal
	work          map[string]*ScopedWorkExport
	coverageRows  []*ScopedSourceCoverageExport
	batches       map[ScopedViewKey][]*ScopedBatchExport
	bindings      []*FederationBindingExport
	enrollments   []*FederationEnrollmentExport
	instance      string
	nativePending map[string]*PendingClaimRequestExport
}

// ValidateScopedImportGraph validates the complete staged owner snapshot before
// either native backend opens its replacement transaction. No prepared pointer
// becomes active, no root is inferred, and no retained bytes are regenerated.
func ValidateScopedImportGraph(records []ImportRecord) error {
	for _, record := range records {
		if IsScopedImportRecord(record) {
			if err := ValidateScopedImportRecord(record); err != nil {
				return err
			}
		}
	}
	return validateScopedImportGraph(records)
}

// Both public entry points validate row shape before this shared relationship
// pass. Keep direct graph callers strict without decoding every row twice.
func validateScopedImportGraph(records []ImportRecord) error {
	g := scopedImportGraph{projects: map[string]*ProjectExport{}, projectIDs: map[int64]string{}, issues: map[string]*IssueExport{}, views: map[string]*ScopedViewExport{}, generations: map[ScopedViewKey]*ScopedGenerationExport{}, descriptors: map[ScopedViewKey]ScopedViewDescriptor{}, ownership: map[string]*ScopedProjectExport{}, cuts: map[ScopedViewKey]map[string]ScopedCut{}, cutovers: map[string]*ScopedProtocolCutoverExport{}, conversions: map[string]*ScopedConversionExport{}, operations: map[string]*ScopedSourceOperationExport{}, submissions: map[string]SourceSubmission{}, originals: map[string]*ScopedPrivateSourceExport{}, dependencies: map[scopedDependencyKey]*ScopedSourceDependencyExport{}, expectations: map[string]*ScopedMutationExpectationExport{}, incarnations: map[scopedPredicateKey]*ScopedPredicateIncarnationExport{}, deliveries: map[ScopedDeliveryRef]*ScopedDeliveryRecord{}, cursors: map[ScopedDeliveryStream]*ScopedDeliveryCursor{}, controls: map[string]*ScopedControlCustodyExport{}, principalKeys: map[string]CustodyPrincipal{}, work: map[string]*ScopedWorkExport{}, nativePending: map[string]*PendingClaimRequestExport{}, batches: map[ScopedViewKey][]*ScopedBatchExport{}}
	seen := map[string]bool{}
	hasScoped := false
	for _, record := range records {
		if IsScopedImportRecord(record) {
			hasScoped = true
			key := scopedImportIdentity(record)
			if seen[key] {
				return errors.New("duplicate scoped primary identity")
			}
			seen[key] = true
		}
		switch r := record.(type) {
		case *MetaKV:
			if r.Key == "instance_uid" {
				g.instance = r.Value
			}
		case *FederationBindingExport:
			g.bindings = append(g.bindings, r)
		case *FederationEnrollmentExport:
			g.enrollments = append(g.enrollments, r)
		case *ProjectExport:
			g.projects[r.UID] = r
			g.projectIDs[r.ID] = r.UID
		case *IssueExport:
			g.issues[r.UID] = r
		case *RootKeyPin:
			g.pins = append(g.pins, *r)
		case *PendingClaimRequestExport:
			g.nativePending[r.RequestUID] = r
		case *ScopedViewExport:
			g.views[r.ViewUID] = r
			principal := CustodyPrincipal{AuthorityUID: r.AuthorityUID, BindingUID: r.BindingUID, Epoch: r.Epoch, AccountableActor: r.AccountableActor, RouteDigest: r.RouteDigest}
			key, err := ScopedPrincipalKey(principal)
			if err != nil {
				return err
			}
			g.principalKeys[key] = principal
		case *ScopedGenerationExport:
			key := ScopedViewKey{r.ViewUID, r.Generation}
			g.generations[key] = r
			var v ScopedViewDescriptor
			if err := scopedJSON(r.DescriptorBytes, &v); err != nil {
				return err
			}
			g.descriptors[key] = v
		case *ScopedProjectExport:
			g.ownership[r.ProjectUID] = r
		case *ScopedProtocolCutoverExport:
			g.cutovers[r.CutoverUID] = r
		case *ScopedConversionExport:
			g.conversions[r.ConversionUID] = r
		case *ScopedSourceOperationExport:
			submission, err := DecodeScopedSourceSubmission(r.ManifestBytes)
			if err != nil {
				return err
			}
			g.operations[r.OperationUID] = r
			g.submissions[r.OperationUID] = submission
			g.principalKeys[r.PrincipalKey] = submission.Operation.Principal
		case *ScopedPrivateSourceExport:
			g.originals[r.EventUID] = r
		case *ScopedSourceDependencyExport:
			g.dependencies[scopedDependencyKey{r.OperationUID, r.ProjectUID, r.IssueUID}] = r
		case *ScopedMutationExpectationExport:
			g.expectations[r.TokenDigest] = r
		case *ScopedPredicateIncarnationExport:
			g.incarnations[scopedPredicateKey{r.ProjectUID, r.SubjectUID, r.PredicateKind}] = r
		case *ScopedDeliveryRecord:
			g.deliveries[r.ScopedDeliveryRef] = r
		case *ScopedDeliveryCursor:
			g.cursors[r.ScopedDeliveryStream] = r
		case *ScopedControlCustodyExport:
			g.controls[r.RequestUID+":"+r.BindingUID] = r
			var principal CustodyPrincipal
			if err := scopedJSON(r.PrincipalBytes, &principal); err != nil {
				return err
			}
			key, err := ScopedPrincipalKey(principal)
			if err != nil {
				return err
			}
			g.principalKeys[key] = principal
		case *ScopedWorkExport:
			g.work[r.ViewUID] = r
		case *ScopedSourceCoverageExport:
			g.coverageRows = append(g.coverageRows, r)
		case *ScopedSourceCustodyExport:
			principal, _, err := DecodeScopedSourceCustodyPrincipal(r.PrincipalBytes)
			if err != nil {
				return err
			}
			key, err := ScopedPrincipalKey(principal)
			if err != nil {
				return err
			}
			g.principalKeys[key] = principal
		}
	}
	if !hasScoped {
		// Negotiated v2 cannot bypass this graph by omitting the new namespace.
		for _, record := range records {
			switch r := record.(type) {
			case *FederationBindingExport:
				if r.RelayConfig != nil && r.RelayConfig.ProtocolVersion == ScopedRelayProtocolVersion {
					return errors.New("v2 binding lacks scoped ownership graph")
				}
			case *FederationEnrollmentExport:
				if r.RelayProtocolVersion == ScopedRelayProtocolVersion {
					return errors.New("v2 enrollment lacks scoped ownership graph")
				}
			}
		}
		return nil
	}
	if !uid.Valid(g.instance) {
		return errors.New("scoped owner snapshot requires its instance UID")
	}
	if err := g.validatePins(records); err != nil {
		return err
	}
	if err := g.validateViews(); err != nil {
		return err
	}
	if err := g.validateOperations(records); err != nil {
		return err
	}
	if err := g.validateObjects(records); err != nil {
		return err
	}
	if err := g.validateRecordInventory(records); err != nil {
		return err
	}
	if err := g.validateDelivery(); err != nil {
		return err
	}
	if err := g.validateNegotiation(records); err != nil {
		return err
	}
	return g.validateQuotas(records)
}
func scopedImportIdentity(record ImportRecord) string {
	var key string
	switch r := record.(type) {
	case *ScopedViewExport:
		key = r.ViewUID
	case *ScopedGenerationExport:
		key = fmt.Sprintf("%s/%d", r.ViewUID, r.Generation)
	case *ScopedProjectExport:
		key = r.ProjectUID
	case *ScopedRecordExport:
		key = fmt.Sprintf("%s/%d/%q/%q", r.ViewUID, r.Generation, r.Collection, r.RecordKey)
	case *ScopedBatchExport:
		key = fmt.Sprintf("%s/%d/%d", r.ViewUID, r.Generation, r.Sequence)
	case *ScopedWorkExport:
		key = r.ViewUID
	case *ScopedDeliveryRecord:
		key = fmt.Sprintf("%s/%d/%s/%s/%d", r.BindingUID, r.Epoch, r.Direction, r.Channel, r.Sequence)
	case *ScopedDeliveryCursor:
		key = fmt.Sprintf("%s/%d/%s/%s", r.BindingUID, r.Epoch, r.Direction, r.Channel)
	case *ScopedSourceOperationExport:
		key = r.OperationUID
	case *ScopedPrivateSourceExport:
		key = r.EventUID
	case *ScopedSourceCustodyExport:
		key = fmt.Sprintf("%s/%s/%d/%s", r.OperationUID, r.BindingUID, r.Epoch, r.Direction)
	case *ScopedSourceDependencyExport:
		key = r.OperationUID + "/" + r.ProjectUID + "/" + r.IssueUID
	case *ScopedSourceCoverageExport:
		key = fmt.Sprintf("%s/%s/%d/%s", r.OperationUID, r.ViewUID, r.Generation, r.CutDigest)
	case *ScopedControlCustodyExport:
		key = r.RequestUID + "/" + r.BindingUID
	case *ScopedMutationExpectationExport:
		key = r.TokenDigest
	case *ScopedPredicateIncarnationExport:
		key = r.ProjectUID + "/" + r.SubjectUID + "/" + r.PredicateKind
	case *ScopedCanonicalCloseExport:
		key = r.IssueUID
	case *ScopedProtocolCutoverExport:
		key = r.CutoverUID
	case *ScopedConversionExport:
		key = r.ConversionUID
	case *ScopedConversionObligationExport:
		key = r.ConversionUID + "/" + r.ObligationUID
	}
	return record.ImportKind() + ":" + key
}
func (g *scopedImportGraph) validatePins(records []ImportRecord) error {
	pins, err := scopedTrustedPins(g.pins)
	if err != nil {
		return err
	}
	for _, record := range records {
		meta, ok := record.(*MetaKV)
		if !ok || !strings.HasPrefix(meta.Key, RootKeyTransitionMetadataPrefix) {
			continue
		}
		var t RootKeyTransition
		if err := json.Unmarshal([]byte(meta.Value), &t, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		previous, err := pins.get(t.Next.ProjectUID, t.PreviousKeyID, t.Next.AuthorityUID)
		if err != nil {
			return err
		}
		next, err := pins.get(t.Next.ProjectUID, t.Next.KeyID, t.Next.AuthorityUID)
		if err != nil {
			return err
		}
		if !bytes.Equal(next.PublicKey, t.Next.PublicKey) || meta.Key != RootKeyTransitionMetadataKey(t) {
			return errors.New("scoped verification history differs from enrolled native keys")
		}
		if err := VerifyRootKeyTransition(previous, t); err != nil {
			return err
		}
	}
	// Explicit native repinning retains independently enrolled historical keys
	// without a transition. Do not invent a transition or infer trust from a DTO;
	// the native pin rows, already validated by provenance replay, are the trust set.
	return nil
}
func (g *scopedImportGraph) validateViews() error {
	trusted, err := scopedTrustedPins(g.pins)
	if err != nil {
		return err
	}
	for key, row := range g.generations {
		view := g.views[key.ViewUID]
		v := g.descriptors[key]
		if view == nil || v.AuthorityUID != view.AuthorityUID || v.DestinationUID != view.DestinationUID || v.RouteDigest != view.RouteDigest {
			return errors.New("orphan scoped generation or changed authenticated view tuple")
		}
		found := false
		for _, hop := range v.Route {
			if hop.BindingUID == view.BindingUID && hop.Epoch == view.Epoch {
				found = true
			}
		}
		if !found {
			return errors.New("generation route lacks indexed authenticated binding/epoch")
		}
		if err := scopedExpectedView(v, v, trusted); err != nil {
			return err
		}
		for _, p := range v.Projects {
			if g.projects[p.ProjectUID] == nil {
				return errors.New("generation project missing from owner snapshot")
			}
		}
		cutover := g.cutovers[row.RootCutoverUID]
		if cutover == nil {
			return errors.New("generation lacks retained root cutover")
		}
		var cert ScopedProtocolCutoverCertificate
		if err := scopedJSON(cutover.CertificateBytes, &cert); err != nil {
			return err
		}
		if cert.Cutover.AuthorityUID != v.AuthorityUID {
			return errors.New("generation cutover authority differs")
		}
		for _, p := range v.Projects {
			if !slices.Contains(cert.Cutover.ProjectUIDs, p.ProjectUID) {
				return errors.New("generation exceeds root cutover scope")
			}
		}
		if row.CheckpointBytes != nil {
			var c ScopedCheckpoint
			if err := scopedJSON(row.CheckpointBytes, &c); err != nil {
				return err
			}
			if err := VerifyScopedCheckpoint(c, v, g.pins); err != nil {
				return err
			}
			g.cuts[key] = map[string]ScopedCut{c.Cut.Digest: c.Cut}
		} else {
			g.cuts[key] = map[string]ScopedCut{}
		}
	}
	for uid, row := range g.views {
		if row.AuthorityUID == g.instance && g.work[uid] == nil {
			return errors.New("root scoped view lacks capture work")
		}
		var selected *ScopedGenerationExport
		var highest int64
		for key, gen := range g.generations {
			if key.ViewUID == uid && key.Generation > highest {
				highest = key.Generation
				selected = gen
			}
		}
		if selected == nil {
			return errors.New("view lacks any retained generation")
		}
		if row.ActiveGeneration != nil {
			active := g.generations[ScopedViewKey{uid, *row.ActiveGeneration}]
			if active == nil || active.State != "active" || active.CheckpointBytes == nil {
				return errors.New("view active pointer is not a complete active generation")
			}
			selected = active
		}
		if row.PreparedGeneration != nil {
			prepared := g.generations[ScopedViewKey{uid, *row.PreparedGeneration}]
			if prepared == nil || prepared.State != "prepared" {
				return errors.New("view preparation pointer missing or wrong state")
			}
			selected = prepared
		}
		if row.State == "active" && row.ActiveGeneration == nil || row.State == "prepared" && row.PreparedGeneration == nil {
			return errors.New("serving/preparing view lacks exact pointer")
		}
		if g.descriptors[ScopedViewKey{uid, selected.Generation}].RequestDigest != row.RequestDigest {
			return errors.New("view request summary differs from chosen retained generation")
		}
		for key, gen := range g.generations {
			if key.ViewUID != uid {
				continue
			}
			if gen.State == "active" && (row.ActiveGeneration == nil || key.Generation != *row.ActiveGeneration) || gen.State == "prepared" && (row.PreparedGeneration == nil || key.Generation != *row.PreparedGeneration) {
				return errors.New("extra active/prepared generation outside view pointer")
			}
		}
	}
	for project, row := range g.ownership {
		if g.projects[project] == nil {
			return errors.New("scoped owner project missing")
		}
		if row.ProtocolFloor == ScopedRelayProtocolVersion {
			cutover := g.cutovers[*row.RootCutoverUID]
			if cutover == nil {
				return errors.New("project floor has no cutover certificate")
			}
			var cert ScopedProtocolCutoverCertificate
			if err := scopedJSON(cutover.CertificateBytes, &cert); err != nil {
				return err
			}
			if !slices.Contains(cert.Cutover.ProjectUIDs, project) {
				return errors.New("project floor outside root cutover")
			}
		}
		if row.OwnerViewUID != nil {
			key := ScopedViewKey{*row.OwnerViewUID, *row.OwnerGeneration}
			v, ok := g.descriptors[key]
			view := g.views[key.ViewUID]
			if !ok || view == nil || scopedProjectInView(v, project) != nil || row.RootCutoverUID == nil || *row.RootCutoverUID != v.RootCutoverUID || row.ProtocolFloor != ScopedRelayProtocolVersion {
				return errors.New("project owner differs from exact scoped generation/floor/cutover")
			}
			if !row.ServingFenced && (view.State != "active" || view.ActiveGeneration == nil || *view.ActiveGeneration != key.Generation) {
				return errors.New("unfenced project owner points at non-serving generation")
			}
		}
		if row.ConversionUID != nil && g.conversions[*row.ConversionUID] == nil {
			return errors.New("project conversion missing")
		}
	}
	return nil
}

func (g *scopedImportGraph) validateOperations(records []ImportRecord) error {
	trusted, err := scopedTrustedPins(g.pins)
	if err != nil {
		return err
	}
	retries := map[string]bool{}
	canonical := map[string][]*EventExport{}
	receipts := map[string][]*AttributionReceipt{}
	for _, record := range records {
		switch row := record.(type) {
		case *EventExport:
			canonical[row.UID] = append(canonical[row.UID], row)
		case *AttributionReceipt:
			receipts[row.EventUID] = append(receipts[row.EventUID], row)
		}
	}
	checkCanonical := func(expected RemoteEvent) error {
		for _, stored := range canonical[expected.EventUID] {
			created, err := time.Parse(time.RFC3339Nano, stored.CreatedAt)
			if err != nil {
				return err
			}
			actual := RemoteEvent{EventUID: stored.UID, OriginInstanceUID: stored.OriginInstanceUID, ProjectUID: g.projectIDs[stored.ProjectID], ProjectName: stored.ProjectName, IssueUID: stored.IssueUID, RelatedIssueUID: stored.RelatedIssueUID, Type: stored.Type, Actor: stored.Actor, Payload: stored.Payload, HLCPhysicalMS: stored.HLCPhysicalMS, HLCCounter: stored.HLCCounter, ContentHash: stored.ContentHash, CreatedAt: created}
			if err := sameScopedNativeOriginal(expected, actual); err != nil {
				return err
			}
		}
		return nil
	}
	retainedOutputs := map[string]RemoteEvent{}
	for uid, row := range g.operations {
		submission := g.submissions[uid]
		op := submission.Operation
		projects, err := scopedOperationProjects(op)
		if err != nil {
			return err
		}
		for _, project := range projects {
			if g.projects[project] == nil {
				return errors.New("source operation target project missing")
			}
		}
		if row.IdempotencyKey != nil && *row.IdempotencyKey != "" {
			key := fmt.Sprintf("%s/%s/%q", row.SourceInstanceUID, row.PrincipalKey, *row.IdempotencyKey)
			if retries[key] {
				return errors.New("duplicate scoped source idempotency identity")
			}
			retries[key] = true
		}
		for i, member := range op.Members {
			original := g.originals[member.EventUID]
			if original == nil || original.OperationUID == nil || *original.OperationUID != uid || original.Ordinal == nil || *original.Ordinal != int64(i) || !scopedSameOriginal(original, submission, i) {
				return errors.New("incomplete source custody or retained original disagreement")
			}
			if err := checkCanonical(submission.Originals[i]); err != nil {
				return err
			}
		}
		if row.TerminalBytes != nil {
			result, err := DecodeScopedSourceTerminalResult(row.TerminalBytes)
			if err != nil {
				return err
			}
			if err := VerifyScopedSourceResult(result, op, g.pins); err != nil {
				return err
			}
			if op.Version == 3 {
				outputs, err := VerifyScopedImportTerminalOutputs(*row, g.pins, g.instance)
				if err != nil {
					return err
				}
				for _, output := range outputs {
					event := output.Event
					if prior, exists := retainedOutputs[event.EventUID]; exists {
						if err := sameScopedNativeOriginal(event, prior); err != nil {
							return err
						}
					}
					retainedOutputs[event.EventUID] = event
					if source := g.originals[event.EventUID]; source != nil {
						prior, err := DecodeRelaySourceEvent(source.EventBytes)
						if err != nil {
							return err
						}
						if err := sameScopedNativeOriginal(event, prior); err != nil {
							return err
						}
					}
					if err := checkCanonical(event); err != nil {
						return err
					}
					for _, receipt := range receipts[event.EventUID] {
						if !reflect.DeepEqual(*receipt, output.Receipt) {
							return errors.New("root import receipt differs from retained native provenance")
						}
					}
				}
			}
			for _, receipt := range result.Receipts {
				original := g.originals[receipt.EventUID]
				if original != nil && original.ProvenanceBytes != nil {
					var retained AttributionReceipt
					if err := scopedJSON(original.ProvenanceBytes, &retained); err != nil {
						return err
					}
					if !reflect.DeepEqual(retained, receipt) {
						return errors.New("terminal result differs from exact retained native receipt")
					}
				}
			}
		}
		for _, proof := range op.Membership {
			row := g.dependencies[scopedDependencyKey{uid, proof.Endpoint.ProjectUID, proof.Endpoint.IssueUID}]
			if row == nil {
				return errors.New("source membership has no durable dependency")
			}
			var retained ScopedMembershipProof
			if err := scopedJSON(row.ProofBytes, &retained); err != nil {
				return err
			}
			if !reflect.DeepEqual(retained, proof) {
				return errors.New("source dependency proof differs from manifest")
			}
			if proof.Kind == "root_membership" {
				if err := VerifyScopedMembership(proof, op.Principal, g.pins); err != nil {
					return err
				}
			} else {
				creating, ok := g.submissions[proof.CreatingOperationUID]
				if !ok || creating.Operation.Principal != op.Principal || creating.Operation.SourceInstanceUID != op.SourceInstanceUID || len(proof.Signatures) != 0 {
					return errors.New("component creation lacks matching authenticated operation")
				}
				primary := false
				for _, event := range creating.Originals {
					if event.Type != "issue.created" {
						continue
					}
					_, refs, err := EventCreationProvenance(event)
					if err != nil {
						return err
					}
					for _, ref := range refs {
						if ref.Kind == "issue" && ref.ProjectUID == proof.Endpoint.ProjectUID && ref.EntityUID == proof.Endpoint.IssueUID {
							primary = true
						}
					}
				}
				if !primary {
					return errors.New("component dependency lacks actual primary creation")
				}
			}
		}
		for _, expectation := range op.Expectations {
			// Retained staged/negative/nonroot DATA does not confer token authority.
			if g.instance != "" && g.instance != op.Principal.AuthorityUID || row.State != "accepted" {
				continue
			}
			token := g.expectations[ScopedOfferedDigest([]byte(expectation.Token))]
			if token == nil {
				return errors.New("source operation expectation token missing")
			}
			var principal CustodyPrincipal
			if err := scopedJSON(token.PrincipalBytes, &principal); err != nil {
				return err
			}
			if principal != op.Principal || token.ConsumedOperationUID != nil && *token.ConsumedOperationUID != uid {
				return errors.New("token belongs to another principal/accepted operation")
			}
			if row.State == "accepted" && (token.ConsumedOperationUID == nil || *token.ConsumedOperationUID != uid) {
				return errors.New("accepted operation lacks exact consumed-token binding")
			}
		}
	}
	for _, original := range g.originals {
		if g.projects[original.ProjectUID] == nil {
			return errors.New("private original project missing")
		}
		if original.OperationUID != nil {
			op, ok := g.submissions[*original.OperationUID]
			if !ok || original.Ordinal == nil || !scopedSameOriginal(original, op, int(*original.Ordinal)) {
				return errors.New("orphan private source member")
			}
		} else if original.ConversionUID == nil {
			return errors.New("ungrouped private original lacks frozen conversion inventory")
		}
		if original.ConversionUID != nil && g.conversions[*original.ConversionUID] == nil {
			return errors.New("private original conversion missing")
		}
		if original.ProvenanceBytes != nil {
			var receipt AttributionReceipt
			if err := scopedJSON(original.ProvenanceBytes, &receipt); err != nil {
				return err
			}
			pin, err := trusted.get(receipt.ProjectUID, receipt.KeyID, receipt.AuthorityUID)
			if err != nil {
				return err
			}
			if err := VerifyRootReceipt(pin, receipt); err != nil {
				return err
			}
		}
	}
	for _, record := range records {
		switch r := record.(type) {
		case *ScopedSourceCustodyExport:
			op, ok := g.submissions[r.OperationUID]
			if !ok || r.ManifestDigest != op.Operation.Digest {
				return errors.New("custody has no matching complete source manifest")
			}
			p, evidence, err := DecodeScopedSourceCustodyPrincipal(r.PrincipalBytes)
			if err != nil {
				return err
			}
			if p.AuthorityUID != op.Operation.Principal.AuthorityUID || p.RouteDigest != op.Operation.Principal.RouteDigest {
				return errors.New("custody principal differs from immutable source authority")
			}
			var exactView *ScopedViewKey
			if evidence != nil {
				exactView = &evidence.View
			}
			if !g.retainedHopPrincipalInView(p, exactView) {
				return errors.New("custody lacks its own retained authenticated hop principal")
			}
			if evidence != nil {
				generation := g.generations[evidence.View]
				view, exists := g.descriptors[evidence.View]
				if !exists || generation == nil || generation.DescriptorDigest != evidence.DescriptorDigest || ScopedOfferedDigest(generation.DescriptorBytes) != evidence.DescriptorDigest || view.AuthorityUID != p.AuthorityUID || view.RouteDigest != p.RouteDigest || !scopedDescriptorHasHop(view, p.BindingUID, p.Epoch, "") || !scopedDescriptorHasHop(view, op.Operation.Principal.BindingUID, op.Operation.Principal.Epoch, op.Operation.SourceInstanceUID) {
					return errors.New("custody differs from its exact retained admitted view")
				}
				selected := map[string]bool{}
				for _, project := range view.Projects {
					selected[project.ProjectUID] = true
				}
				for _, member := range op.Operation.Members {
					if !selected[member.ProjectUID] {
						return errors.New("custody member outside its admitted view")
					}
				}
				if op.Operation.Version == 3 && !selected[op.Operation.ImportIntent.ProjectUID] {
					return errors.New("custody import intent outside its admitted view")
				}
			}
			if r.ResultAcknowledged && g.operations[r.OperationUID].TerminalBytes == nil {
				return errors.New("custody result ACK without retained root disposition")
			}
		case *ScopedSourceDependencyExport:
			op, ok := g.submissions[r.OperationUID]
			if !ok {
				return errors.New("dependency operation missing")
			}
			found := false
			for _, proof := range op.Operation.Membership {
				if proof.Endpoint == (ScopedEndpoint{r.ProjectUID, r.IssueUID}) {
					found = true
				}
			}
			if !found {
				return errors.New("dependency not declared in immutable source manifest")
			}
			if r.EstablishingOperationUID != nil {
				establishing := g.operations[*r.EstablishingOperationUID]
				if establishing == nil {
					return errors.New("establishing operation missing")
				}
				if r.State == "satisfied" && establishing.State != "accepted" || r.State == "rejected" && establishing.State != "rejected" || r.State == "canceled" && establishing.State != "canceled" {
					return errors.New("dependency outcome disagrees with establishing operation")
				}
			}
		case *ScopedMutationExpectationExport:
			current := g.incarnations[scopedPredicateKey{r.ProjectUID, r.SubjectUID, r.PredicateKind}]
			var predicate ScopedMutationPredicate
			if err := scopedJSON(r.PredicateBytes, &predicate); err != nil {
				return err
			}
			if current == nil || predicate.Incarnation > current.Incarnation || predicate.Incarnation == current.Incarnation && predicate.CanonicalDigest != current.CanonicalDigest {
				return errors.New("token predicate missing, future, or same-incarnation digest mismatch")
			}
			if r.ConsumedOperationUID != nil {
				op, ok := g.submissions[*r.ConsumedOperationUID]
				if !ok {
					return errors.New("consumed token operation missing")
				}
				found := false
				for _, e := range op.Operation.Expectations {
					if ScopedOfferedDigest([]byte(e.Token)) == r.TokenDigest {
						found = true
					}
				}
				if !found || g.operations[op.Operation.UID].State != "accepted" {
					return errors.New("consumed token lacks exact accepted manifest")
				}
			}
		}
	}
	return nil
}

// Use the existing native whole-original commitment over the actual typed
// contents of BOTH copies, including origin/project/HLC/payload. SQL-local row
// identities do not enter that commitment. Exact private source bytes remain
// retained separately; portable JSON may normalize payload whitespace on replay.
func sameScopedNativeOriginal(expected, actual RemoteEvent) error {
	if expected.EventUID != actual.EventUID || expected.ContentHash != actual.ContentHash {
		return errors.New("retained native original identity disagrees")
	}
	hash, err := RemoteEventContentHash(actual)
	if err != nil {
		return err
	}
	if hash != expected.ContentHash {
		return errors.New("retained native original content disagrees")
	}
	return nil
}

func (g *scopedImportGraph) validateObjects(records []ImportRecord) error {
	batches := g.batches
	for _, record := range records {
		if row, ok := record.(*ScopedBatchExport); ok {
			key := ScopedViewKey{row.ViewUID, row.Generation}
			v, ok := g.descriptors[key]
			if !ok {
				return errors.New("batch generation missing")
			}
			var b ScopedBatch
			if err := scopedJSON(row.BatchBytes, &b); err != nil {
				return err
			}
			if err := VerifyScopedBatch(b, v, g.pins); err != nil {
				return err
			}
			if _, duplicate := g.cuts[key][b.Cut.Digest]; duplicate {
				return errors.New("duplicate scoped cut digest")
			}
			g.cuts[key][b.Cut.Digest] = b.Cut
			batches[key] = append(batches[key], row)
		}
	}
	for key, rows := range batches {
		slices.SortFunc(rows, func(a, b *ScopedBatchExport) int {
			if a.Sequence < b.Sequence {
				return -1
			}
			if a.Sequence > b.Sequence {
				return 1
			}
			return 0
		})
		generation := g.generations[key]
		previous := generation.CutDigest
		sequence := generation.CutSequence
		for _, row := range rows {
			if row.Sequence <= sequence {
				continue
			} // Retained earlier history remains independently signed.
			if sequence == int64(^uint64(0)>>1) || row.Sequence != sequence+1 || row.PreviousDigest != previous {
				return errors.New("missing scoped batch chain or prior cut")
			}
			previous, sequence = row.Digest, row.Sequence
		}
	}
	coverage := map[string]bool{}
	for _, record := range records {
		switch r := record.(type) {
		case *ScopedProtocolCutoverExport:
			var projects []string
			if err := scopedJSON(r.ProjectUIDsBytes, &projects); err != nil {
				return err
			}
			authority := ""
			for _, project := range projects {
				if g.projects[project] == nil {
					return errors.New("root cutover project missing")
				}
				for _, pin := range g.pins {
					if pin.ProjectUID == project {
						if authority != "" && authority != pin.AuthorityUID {
							return errors.New("root cutover enrolled authorities disagree")
						}
						authority = pin.AuthorityUID
					}
				}
			}
			expected := ScopedProtocolCutoverExpectation{UID: r.CutoverUID, AuthorityUID: authority, RequestDigest: r.RequestDigest, ProjectUIDs: projects, Floor: int(r.Floor), ReadinessDigest: ScopedOfferedDigest(r.ReadinessBytes), SuspendedBindingsDigest: ScopedOfferedDigest(r.SuspendedBindingsBytes)}
			var c ScopedProtocolCutoverCertificate
			if err := scopedJSON(r.CertificateBytes, &c); err != nil {
				return err
			}
			if err := VerifyScopedCutoverCertificate(c, expected, g.pins); err != nil {
				return err
			}
			// T13 must add its frozen typed readiness/suspension inventory semantics
			// before runtime conversion or restore activation. Exact bodies/commitments
			// and this certificate's crypto are fully verified here.
		case *ScopedSourceCoverageExport:
			op, ok := g.submissions[r.OperationUID]
			if !ok || g.operations[r.OperationUID].State != "accepted" {
				return errors.New("coverage lacks accepted complete source operation")
			}
			key := ScopedViewKey{r.ViewUID, r.Generation}
			v, ok := g.descriptors[key]
			if !ok {
				return errors.New("coverage generation missing")
			}
			cut, ok := g.cuts[key][r.CutDigest]
			if !ok {
				return errors.New("coverage lacks exact retained signed cut")
			}
			var c SourceCoverage
			if err := scopedJSON(r.CertificateBytes, &c); err != nil {
				return err
			}
			if err := VerifyScopedSourceCoverage(c, op.Operation, v, cut, g.pins); err != nil {
				return err
			}
			coverage[r.OperationUID] = true
		case *ScopedRecordExport:
			key := ScopedViewKey{r.ViewUID, r.Generation}
			v, ok := g.descriptors[key]
			if !ok || scopedProjectInView(v, r.ProjectUID) != nil {
				return errors.New("retained projection row outside exact generation")
			}
			var effect ScopedEffect
			if err := scopedJSON(r.RecordBytes, &effect); err != nil {
				return err
			}
			if effect.Action == "replace" {
				c := scopedEffectCollections(effect)
				trusted, err := scopedTrustedPins(g.pins)
				if err != nil {
					return err
				}
				if err := verifyScopedCollections(v, c, trusted); err != nil {
					return err
				}
			}
		case *ScopedWorkExport:
			view := g.views[r.ViewUID]
			if view == nil {
				return errors.New("capture work view missing")
			}
			if r.CaptureBytes != nil {
				capture, err := DecodeScopedCanonicalCapture(r.CaptureBytes)
				if err != nil {
					return err
				}
				if capture.CaptureUID != *r.CaptureUID {
					return errors.New("private capture UID/index disagreement")
				}
				matched := false
				for key, descriptor := range g.descriptors {
					if key.ViewUID != r.ViewUID || len(capture.ProjectCuts) != len(descriptor.Projects) {
						continue
					}
					exact := true
					for _, project := range descriptor.Projects {
						if _, ok := capture.ProjectCuts[project.ProjectUID]; !ok {
							exact = false
							break
						}
					}
					if exact {
						matched = true
						break
					}
				}
				if !matched {
					return errors.New("capture project cut inventory matches no retained descriptor of its authenticated view")
				}

			}
		case *ScopedControlCustodyExport:
			var request ScopedControlRequest
			if err := scopedJSON(r.RequestBytes, &request); err != nil {
				return err
			}
			if g.projects[request.Subject.ProjectUID] == nil {
				return errors.New("control project missing")
			}
			var p CustodyPrincipal
			if err := scopedJSON(r.PrincipalBytes, &p); err != nil {
				return err
			}
			if native := g.nativePending[r.RequestUID]; native != nil {
				if native.IssueUID != request.Subject.IssueUID || g.projectIDs[native.ProjectID] != request.Subject.ProjectUID || native.Holder != request.Principal.Holder || native.HolderInstanceUID != request.Principal.HolderInstanceUID || native.ClientKind != request.Principal.ClientKind {
					return errors.New("control custody disagrees with native pending-claim identity")
				}
			}
			if r.ConversionUID != nil && g.conversions[*r.ConversionUID] == nil {
				return errors.New("control conversion missing")
			}
		case *ScopedPredicateIncarnationExport:
			if g.projects[r.ProjectUID] == nil {
				return errors.New("predicate owner project missing")
			}
		case *ScopedCanonicalCloseExport:
			issue := g.issues[r.IssueUID]
			if issue == nil || g.projectIDs[issue.ProjectID] != r.ProjectUID {
				return errors.New("canonical close owner issue missing")
			}
			proof, err := scopedCanonicalCloseRow(r)
			if err != nil {
				return err
			}
			if err := ValidateScopedCanonicalCloseState(proof, issue.UID, issue.Status); err != nil {
				return err
			}
			if proof.Value != nil {
				peer := g.issues[proof.Value.PeerIssueUID]
				if peer == nil {
					// Incoming materialization may precede peer originals. Its retained
					// owner must name this receiver, not grant it root authority.
					owner := g.ownership[r.ProjectUID]
					if owner == nil || owner.OwnerViewUID == nil || owner.OwnerGeneration == nil {
						return errors.New("canonical close peer missing or belongs to another project")
					}
					incoming := g.descriptors[ScopedViewKey{*owner.OwnerViewUID, *owner.OwnerGeneration}]
					if incoming.DestinationUID != g.instance || incoming.AuthorityUID == g.instance {
						return errors.New("canonical close peer missing or belongs to another project")
					}
				} else if g.projectIDs[peer.ProjectID] != proof.Value.PeerProjectUID {
					return errors.New("canonical close peer missing or belongs to another project")
				}
			}
		case *ScopedConversionExport:
			var request ScopedConversionRequest
			if err := scopedJSON(r.RequestBytes, &request); err != nil {
				return err
			}
			key := request.Target.Key
			v, ok := g.descriptors[key]
			if !ok || !reflect.DeepEqual(v, request.Target) {
				return errors.New("conversion target has no exact retained generation")
			}
			var old []ScopedOldBinding
			if err := scopedJSON(r.OldBindingsBytes, &old); err != nil {
				return err
			}
			for _, binding := range old {
				if g.projects[binding.ProjectUID] == nil {
					return errors.New("old conversion project missing")
				}
			}
			if r.RootCutoverUID != nil && g.cutovers[*r.RootCutoverUID] == nil {
				return errors.New("conversion root cutover missing")
			}
		case *ScopedConversionObligationExport:
			if g.conversions[r.ConversionUID] == nil {
				return errors.New("conversion obligation lacks frozen conversion/principal")
			}
			if !g.obligationPresent(r) {
				return errors.New("conversion obligation object missing or digest differs")
			}
		}
	}
	for uid, row := range g.operations {
		if row.State == "accepted" && (row.OverlayState == "settled" || row.OverlayState == "accepted_private") && !coverage[uid] {
			return errors.New("accepted source overlay settled without private coverage")
		}
	}
	return nil
}

func (g *scopedImportGraph) obligationPresent(r *ScopedConversionObligationExport) bool {
	principal, ok := g.principalKeys[r.PrincipalKey]
	if !ok || principal.BindingUID != r.BindingUID || principal.Epoch != r.Epoch {
		return false
	}
	switch r.Kind {
	case "source":
		row := g.operations[r.ObjectUID]
		return row != nil && row.Digest == r.Digest && row.PrincipalKey == r.PrincipalKey
	case "result":
		row := g.operations[r.ObjectUID]
		if row == nil || row.TerminalBytes == nil {
			return false
		}
		result, err := DecodeScopedSourceTerminalResult(row.TerminalBytes)
		if err != nil {
			return false
		}
		digest, err := ScopedSourceResultObjectDigest(result)
		return err == nil && digest == r.Digest && row.PrincipalKey == r.PrincipalKey
	case "checkpoint":
		for key, row := range g.generations {
			if key.ViewUID == r.ObjectUID && row.CutDigest == r.Digest {
				return true
			}
		}
	case "capture":
		for _, work := range g.work {
			view := g.views[work.ViewUID]
			if view == nil || work.CaptureUID == nil || work.CaptureDigest == nil || work.CaptureBytes == nil {
				continue
			}
			if *work.CaptureUID == r.ObjectUID && *work.CaptureDigest == r.Digest && ScopedOfferedDigest(work.CaptureBytes) == r.Digest && view.BindingUID == principal.BindingUID && view.Epoch == principal.Epoch && view.AuthorityUID == principal.AuthorityUID && view.AccountableActor == principal.AccountableActor && view.RouteDigest == principal.RouteDigest {
				return true
			}
		}
	case "conversion":
		row := g.conversions[r.ObjectUID]
		if row == nil {
			return false
		}
		var request ScopedConversionRequest
		if scopedJSON(row.RequestBytes, &request) != nil {
			return false
		}
		digest, err := ScopedConversionRequestObjectDigest(request)
		if err == nil && digest == r.Digest {
			return true
		}
		if row.ResultBytes != nil {
			var state ScopedConversionState
			if scopedJSON(row.ResultBytes, &state) != nil {
				return false
			}
			digest, err = ScopedConversionStateObjectDigest(state)
			return err == nil && digest == r.Digest
		}
	case "control":
		row := g.controls[r.ObjectUID+":"+r.BindingUID]
		if row == nil {
			return false
		}
		var request ScopedControlRequest
		if scopedJSON(row.RequestBytes, &request) != nil {
			return false
		}
		digest, err := ScopedControlRequestObjectDigest(request)
		if err == nil && digest == r.Digest {
			return true
		}
		if row.ResultBytes != nil {
			var result ScopedControlResult
			if scopedJSON(row.ResultBytes, &result) != nil {
				return false
			}
			digest, err = ScopedControlResultObjectDigest(result)
			return err == nil && digest == r.Digest
		}
	case "coverage":
		for _, row := range g.coverageRows {
			if row.OperationUID != r.ObjectUID {
				continue
			}
			var coverage SourceCoverage
			if scopedJSON(row.CertificateBytes, &coverage) != nil {
				return false
			}
			digest, err := ScopedSourceCoverageObjectDigest(coverage)
			if err == nil && digest == r.Digest {
				return true
			}
		}
	case "delivery":
		for ref, row := range g.deliveries {
			if ref.BindingUID == r.BindingUID && ref.Epoch == r.Epoch && ref.Sequence == r.Sequence && row.ObjectUID == r.ObjectUID && row.OfferedDigest == r.Digest {
				return true
			}
		}
	}
	return false
}

func (g *scopedImportGraph) validateDelivery() error {
	for ref, row := range g.deliveries {
		if g.cursors[ref.ScopedDeliveryStream] == nil {
			return errors.New("scoped delivery stream cursor missing")
		}
		if row.Upstream != nil {
			upstream := g.deliveries[*row.Upstream]
			if upstream == nil || !upstream.Accepted || upstream.ObjectUID != row.ObjectUID || upstream.ObjectDigest != row.ObjectDigest {
				return errors.New("forwarded delivery lacks exact upstream hop custody")
			}
		}
		var header ScopedDeliveryHeader
		var body []byte
		var err error
		if row.EnvelopeBytes != nil {
			header, body, err = DecodeScopedDelivery(row.EnvelopeBytes)
		} else {
			err = scopedJSON(row.HeaderBytes, &header)
		}
		if err != nil {
			return err
		}
		if row.Direction == ScopedDeliveryInbound && header.ReceiverInstanceUID != g.instance || row.Direction == ScopedDeliveryOutbound && header.SenderInstanceUID != g.instance {
			return errors.New("scoped delivery direction differs from local owner")
		}
		if row.EnvelopeBytes != nil {
			if row.Direction == ScopedDeliveryInbound && !row.Accepted && (header.Channel == ScopedDeliverySource || header.Channel == ScopedDeliveryControl || header.Channel == ScopedDeliveryCoverage) {
				if err := g.validateStagedInbound(header, body); err != nil {
					return err
				}
			} else if err := g.validateDeliveryBody(header, body); err != nil {
				return err
			}
		} else {
			// Compaction preserves both the exact object and its authenticated hop.
			if !g.retainedDeliveryObject(header) {
				return errors.New("compacted offer has no retained independent object obligation")
			}
		}
	}
	for stream, cursor := range g.cursors {
		phases := []struct {
			phase  string
			n      int64
			digest string
		}{{"received", cursor.ReceivedSequence, cursor.ReceivedDigest}, {"accepted", cursor.AcceptedSequence, cursor.AcceptedDigest}, {"emitted", cursor.EmittedSequence, cursor.EmittedDigest}, {"acknowledged", cursor.AcknowledgedSequence, cursor.AcknowledgedDigest}}
		for _, phase := range phases {
			if stream.Direction == ScopedDeliveryInbound && (phase.phase == "emitted" || phase.phase == "acknowledged") || stream.Direction == ScopedDeliveryOutbound && (phase.phase == "received" || phase.phase == "accepted") {
				continue
			}
			rows := []ScopedDeliveryRecord{}
			for ref, row := range g.deliveries {
				if ref.ScopedDeliveryStream == stream && ref.Sequence <= phase.n {
					rows = append(rows, *row)
				}
			}
			if err := ValidateScopedDeliveryPrefix(ScopedDeliveryCursor{ScopedDeliveryStream: stream}, phase.n, phase.phase, rows); err != nil {
				return err
			}
			if phase.n > 0 {
				endpoint := g.deliveries[ScopedDeliveryRef{ScopedDeliveryStream: stream, Sequence: phase.n}]
				if endpoint == nil || endpoint.OfferedDigest != phase.digest {
					return errors.New("cursor digest differs from retained endpoint offer")
				}
			}
		}
		// Outbound allocation/insertion is contiguous even beyond emitted prefixes.
		if stream.Direction == ScopedDeliveryOutbound {
			var maxSequence, count int64
			for ref := range g.deliveries {
				if ref.ScopedDeliveryStream == stream {
					count++
					if ref.Sequence > maxSequence {
						maxSequence = ref.Sequence
					}
				}
			}
			if maxSequence != count {
				return errors.New("outbound scoped allocation has a sequence gap")
			}
		}
	}
	return nil
}

// Unaccepted source/control bodies are durable staging, not logical admission.
// Their exact bytes remain owed; native admission/quarantine must run again.
func (g *scopedImportGraph) validateStagedInbound(header ScopedDeliveryHeader, body []byte) error {
	var view *ScopedViewExport
	for _, binding := range g.bindings {
		c := binding.RelayConfig
		if c == nil || c.ProtocolVersion != ScopedRelayProtocolVersion || c.Validate(g.instance) != nil || c.BindingUID != header.BindingUID || c.ResetEpoch != header.Epoch || c.AuthorityUID != header.AuthorityUID || c.UpstreamInstanceUID != header.SenderInstanceUID || header.ReceiverInstanceUID != g.instance || !slices.Equal(c.HubPath[:len(c.HubPath)-1], header.Path) {
			continue
		}
		candidate := g.views[c.ScopedView.ViewUID]
		if candidate != nil && candidate.AuthorityUID == header.AuthorityUID && g.retainedHopPrincipal(CustodyPrincipal{AuthorityUID: candidate.AuthorityUID, BindingUID: c.BindingUID, Epoch: c.ResetEpoch, AccountableActor: binding.Actor, RouteDigest: candidate.RouteDigest}) {
			view = candidate
			break
		}
	}
	if view == nil {
		for _, enrollment := range g.enrollments {
			if enrollment.RelayProtocolVersion != ScopedRelayProtocolVersion || enrollment.RelayBindingUID == nil || *enrollment.RelayBindingUID != header.BindingUID || enrollment.RelayResetEpoch != header.Epoch || enrollment.SpokeInstanceUID != header.SenderInstanceUID || header.ReceiverInstanceUID != g.instance {
				continue
			}
			for _, candidate := range g.views {
				if candidate.AuthorityUID == header.AuthorityUID && g.retainedHopPrincipal(CustodyPrincipal{AuthorityUID: candidate.AuthorityUID, BindingUID: header.BindingUID, Epoch: header.Epoch, AccountableActor: enrollment.Actor, RouteDigest: candidate.RouteDigest}) && g.viewInboundRoute(candidate, header) && (enrollment.RelayServeDownstream || len(header.Path) == 1) {
					view = candidate
					break
				}
			}
		}
	}
	if view == nil && !g.nativeScopedHopPresent(header.BindingUID, header.Epoch) {
		for _, candidate := range g.views {
			if candidate.BindingUID != header.BindingUID || candidate.Epoch != header.Epoch || candidate.AuthorityUID != header.AuthorityUID {
				continue
			}
			principal := CustodyPrincipal{AuthorityUID: candidate.AuthorityUID, BindingUID: candidate.BindingUID, Epoch: candidate.Epoch, AccountableActor: candidate.AccountableActor, RouteDigest: candidate.RouteDigest}
			if g.retainedHopPrincipal(principal) && g.viewInboundRoute(candidate, header) {
				view = candidate
				break
			}
		}
	}
	if view == nil {
		return errors.New("unaccepted scoped offer lacks exact retained authenticated hop/epoch/route")
	}
	var objectUID, digest string
	var err error
	switch header.Channel {
	case ScopedDeliverySource:
		submission, e := DecodeScopedSourceSubmission(body)
		if e != nil {
			return e
		}
		principal := submission.Operation.Principal
		if principal.AuthorityUID != view.AuthorityUID || principal.RouteDigest != view.RouteDigest {
			return errors.New("staged source differs from retained authenticated authority/route")
		}
		if g.operations[submission.Operation.UID] != nil {
			return g.validateDeliveryBody(header, body)
		}
		objectUID, digest = submission.Operation.UID, submission.Operation.Digest
	case ScopedDeliveryCoverage:
		var c SourceCoverage
		if err = scopedJSON(body, &c); err != nil {
			return err
		}
		op, ok := g.submissions[c.OperationUID]
		expected, hasView := g.descriptors[c.View]
		if !ok || !hasView || c.OperationDigest != op.Operation.Digest || c.View.ViewUID != view.ViewUID || expected.AuthorityUID != header.AuthorityUID || expected.RouteDigest != view.RouteDigest || op.Operation.Principal.AuthorityUID != expected.AuthorityUID {
			return errors.New("pending coverage lacks independent operation/view/hop context")
		}
		// Its incoming cut is deliberately NOT an authenticated expected cut.
		// Retain pending bytes only; ordinary admission verifies the certificate
		// once the independently signed cut arrives. No acceptance/ACK follows.
		objectUID = c.OperationUID
		digest, err = ScopedSourceCoverageObjectDigest(c)
	case ScopedDeliveryControl:
		var variant struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		}
		if err = json.Unmarshal(body, &variant); err != nil {
			return err
		}
		if (variant.Kind != "") == (variant.State != "") {
			return errors.New("unknown staged control body variant")
		}
		if variant.Kind != "" {
			var request ScopedControlRequest
			if err = scopedJSON(body, &request); err != nil {
				return err
			}
			if g.controls[request.RequestUID+":"+header.BindingUID] != nil {
				return g.validateDeliveryBody(header, body)
			}
			objectUID = request.RequestUID
			digest, err = ScopedControlRequestObjectDigest(request)
		} else {
			var result ScopedControlResult
			if err = scopedJSON(body, &result); err != nil {
				return err
			}
			if g.controls[result.RequestUID+":"+header.BindingUID] != nil {
				return g.validateDeliveryBody(header, body)
			}
			objectUID = result.RequestUID
			digest, err = ScopedControlResultObjectDigest(result)
		}
	default:
		return errors.New("channel requires retained cryptographic expected context")
	}
	if err != nil {
		return err
	}
	if objectUID != header.ObjectUID || digest != header.ObjectDigest {
		return errors.New("staged object commitment differs from exact typed body")
	}
	return nil
}

func (g *scopedImportGraph) validateDeliveryBody(header ScopedDeliveryHeader, body []byte) error {
	var objectUID, digest string
	var err error
	routeMatches := false
	switch header.Channel {
	case ScopedDeliveryView:
		// Closed structural discriminator: checkpoints carry collections, batches
		// carry effects. It selects a type, never a caller-supplied hash algorithm.
		var variant struct {
			Collections jsontext.Value `json:"collections"`
			Effects     jsontext.Value `json:"effects"`
		}
		if err = json.Unmarshal(body, &variant); err != nil {
			return err
		}
		if (len(variant.Collections) > 0) == (len(variant.Effects) > 0) {
			return errors.New("unknown/ambiguous scoped view body variant")
		}
		if len(variant.Collections) > 0 {
			var c ScopedCheckpoint
			if err = scopedJSON(body, &c); err != nil {
				return err
			}
			expected, ok := g.descriptors[c.View.Key]
			if !ok {
				return errors.New("delivery checkpoint generation missing")
			}
			if err = VerifyScopedCheckpoint(c, expected, g.pins); err != nil {
				return err
			}
			objectUID, digest = c.View.Key.ViewUID, c.Cut.Digest
			routeMatches = scopedDescriptorDeliveryRoute(expected, header)
		} else {
			var b ScopedBatch
			if err = scopedJSON(body, &b); err != nil {
				return err
			}
			expected, ok := g.descriptors[b.View.Key]
			if !ok {
				return errors.New("delivery batch generation missing")
			}
			if err = VerifyScopedBatch(b, expected, g.pins); err != nil {
				return err
			}
			objectUID, digest = b.View.Key.ViewUID, b.Cut.Digest
			routeMatches = scopedDescriptorDeliveryRoute(expected, header)
		}
	case ScopedDeliverySource:
		submission, e := DecodeScopedSourceSubmission(body)
		if e != nil {
			return e
		}
		expected, ok := g.submissions[submission.Operation.UID]
		if !ok || !reflect.DeepEqual(expected.Operation, submission.Operation) || len(expected.OriginalBytes) != len(submission.OriginalBytes) {
			return errors.New("delivery source manifest missing/different")
		}
		for i := range expected.OriginalBytes {
			if !bytes.Equal(expected.OriginalBytes[i], submission.OriginalBytes[i]) {
				return errors.New("delivery native original bytes differ from custody")
			}
		}
		if !bytes.Equal(body, g.operations[submission.Operation.UID].ManifestBytes) {
			return errors.New("delivery source full authenticated bytes differ from retained custody")
		}
		objectUID, digest = submission.Operation.UID, submission.Operation.Digest
		routeMatches = g.principalDeliveryRoute(expected.Operation.Principal, header)
	case ScopedDeliveryResult:
		var result SourceResult
		if err = scopedJSON(body, &result); err != nil {
			return err
		}
		expected, ok := g.submissions[result.OperationUID]
		if !ok {
			return errors.New("delivery result operation missing")
		}
		if err = VerifyScopedSourceResult(result, expected.Operation, g.pins); err != nil {
			return err
		}
		objectUID = result.OperationUID
		digest, err = ScopedSourceResultObjectDigest(result)
		routeMatches = g.principalDeliveryRoute(expected.Operation.Principal, header)
	case ScopedDeliveryCoverage:
		var c SourceCoverage
		if err = scopedJSON(body, &c); err != nil {
			return err
		}
		op, ok := g.submissions[c.OperationUID]
		if !ok {
			return errors.New("delivery coverage operation missing")
		}
		v, ok := g.descriptors[c.View]
		if !ok {
			return errors.New("delivery coverage view missing")
		}
		cut, ok := g.cuts[c.View][c.Cut.Digest]
		if !ok {
			return errors.New("delivery coverage cut missing")
		}
		if err = VerifyScopedSourceCoverage(c, op.Operation, v, cut, g.pins); err != nil {
			return err
		}
		objectUID = c.OperationUID
		digest, err = ScopedSourceCoverageObjectDigest(c)
		routeMatches = scopedDescriptorDeliveryRoute(v, header)
	case ScopedDeliveryControl:
		var variant struct {
			Kind  string `json:"kind"`
			State string `json:"state"`
		}
		if err = json.Unmarshal(body, &variant); err != nil {
			return err
		}
		if (variant.Kind != "") == (variant.State != "") {
			return errors.New("unknown/ambiguous scoped control body variant")
		}
		if variant.Kind != "" {
			var request ScopedControlRequest
			if err = scopedJSON(body, &request); err != nil {
				return err
			}
			row := g.controls[request.RequestUID+":"+header.BindingUID]
			if row == nil || !bytes.Equal(body, row.RequestBytes) {
				return errors.New("delivery control request bytes differ from retained immutable request")
			}
			objectUID = request.RequestUID
			digest, err = ScopedControlRequestObjectDigest(request)
		} else {
			var result ScopedControlResult
			if err = scopedJSON(body, &result); err != nil {
				return err
			}
			row := g.controls[result.RequestUID+":"+header.BindingUID]
			if row == nil || result.Digest != row.RequestDigest {
				return errors.New("delivery control result refers to another immutable request")
			}
			// A historical pending offer can precede the retained terminal. A
			// terminal offer must agree with that one immutable outcome.
			if result.State != "pending" {
				var retained ScopedControlResult
				if err := scopedJSON(row.ResultBytes, &retained); err != nil {
					return err
				}
				if !reflect.DeepEqual(result, retained) {
					return errors.New("delivery control terminal differs from retained outcome")
				}
			}
			objectUID = result.RequestUID
			digest, err = ScopedControlResultObjectDigest(result)
		}
		row := g.controls[objectUID+":"+header.BindingUID]
		if row == nil {
			return errors.New("delivery control custody missing")
		}
		var principal CustodyPrincipal
		if err := scopedJSON(row.PrincipalBytes, &principal); err != nil {
			return err
		}
		routeMatches = g.principalDeliveryRoute(principal, header)
	case ScopedDeliveryConversion:
		var variant struct {
			Cutover jsontext.Value `json:"cutover"`
			Request jsontext.Value `json:"request"`
			Target  jsontext.Value `json:"target"`
		}
		if err = json.Unmarshal(body, &variant); err != nil {
			return err
		}
		count := 0
		for _, v := range []jsontext.Value{variant.Cutover, variant.Request, variant.Target} {
			if len(v) > 0 {
				count++
			}
		}
		if count != 1 {
			return errors.New("unknown/ambiguous scoped conversion body variant")
		}
		if len(variant.Cutover) > 0 {
			var c ScopedProtocolCutoverCertificate
			if err = scopedJSON(body, &c); err != nil {
				return err
			}
			row := g.cutovers[c.Cutover.UID]
			if row == nil {
				return errors.New("delivery cutover row missing")
			}
			var expected ScopedProtocolCutoverCertificate
			if err = scopedJSON(row.CertificateBytes, &expected); err != nil {
				return err
			}
			if err = VerifyScopedCutoverCertificate(c, expected.Cutover, g.pins); err != nil {
				return err
			}
			objectUID = c.Cutover.UID
			digest, err = ScopedCutoverCertificateObjectDigest(c)
			for _, descriptor := range g.descriptors {
				if descriptor.RootCutoverUID == c.Cutover.UID && descriptor.AuthorityUID == c.Cutover.AuthorityUID && scopedDescriptorDeliveryRoute(descriptor, header) {
					routeMatches = true
					break
				}
			}
		} else if len(variant.Request) > 0 {
			var state ScopedConversionState
			if err = scopedJSON(body, &state); err != nil {
				return err
			}
			objectUID = state.Request.UID
			digest, err = ScopedConversionStateObjectDigest(state)
			row := g.conversions[objectUID]
			if row == nil {
				return errors.New("delivery conversion state missing")
			}
			var request ScopedConversionRequest
			if err := scopedJSON(row.RequestBytes, &request); err != nil {
				return err
			}
			phases := []string{"prepare", "fence", "activate", "complete"}
			if !reflect.DeepEqual(state.Request, request) || slices.Index(phases, state.Phase) > slices.Index(phases, row.Phase) {
				return errors.New("delivery conversion state changes retained request or advances unretained phase")
			}
			if state.Phase != "prepare" && (row.RootCutoverUID == nil || state.RootCutoverUID != *row.RootCutoverUID || row.InventoryDigest == nil || state.InventoryDigest != *row.InventoryDigest) || (state.Phase == "activate" || state.Phase == "complete") && (row.BaselineDigest == nil || state.BaselineDigest != *row.BaselineDigest) {
				return errors.New("historical conversion state differs from retained cutover/inventory/baseline")
			}
			var old []ScopedOldBinding
			if err := scopedJSON(row.OldBindingsBytes, &old); err != nil {
				return err
			}
			if !reflect.DeepEqual(state.OldBindings, old) {
				return errors.New("historical conversion old-binding inventory differs")
			}
			routeMatches = scopedDescriptorDeliveryRoute(request.Target, header)
		} else {
			var request ScopedConversionRequest
			if err = scopedJSON(body, &request); err != nil {
				return err
			}
			objectUID = request.UID
			digest, err = ScopedConversionRequestObjectDigest(request)
			row := g.conversions[objectUID]
			if row == nil || !bytes.Equal(body, row.RequestBytes) {
				return errors.New("delivery conversion immutable request differs from retained bytes")
			}
			routeMatches = scopedDescriptorDeliveryRoute(request.Target, header)
		}
	default:
		return errors.New("unknown scoped delivery channel")
	}
	if err != nil {
		return err
	}
	if objectUID != header.ObjectUID || digest != header.ObjectDigest {
		return errors.New("delivery object commitment differs from closed typed body")
	}
	if !routeMatches {
		return errors.New("delivery hop differs from independently retained object route")
	}
	return nil
}
func (g *scopedImportGraph) retainedDeliveryObject(header ScopedDeliveryHeader) bool {
	exactBody := func(body []byte) bool {
		return ScopedOfferedDigest(body) == header.BodyDigest && g.validateDeliveryBody(header, body) == nil
	}
	switch header.Channel {
	case ScopedDeliveryView:
		for key, generation := range g.generations {
			if key.ViewUID != header.ObjectUID {
				continue
			}
			if generation.CheckpointBytes != nil && exactBody(generation.CheckpointBytes) {
				return true
			}
			for _, batch := range g.batches[key] {
				if exactBody(batch.BatchBytes) {
					return true
				}
			}
		}
	case ScopedDeliverySource:
		row := g.operations[header.ObjectUID]
		return row != nil && row.Digest == header.ObjectDigest && exactBody(row.ManifestBytes)
	case ScopedDeliveryResult:
		row := g.operations[header.ObjectUID]
		if row != nil && row.TerminalBytes != nil {
			result, execution, err := decodeScopedSourceTerminal(row.TerminalBytes)
			if err != nil {
				return false
			}
			if execution == nil {
				// Preserve the exact retained legacy/public body, including its
				// original wire representation. It has no private frame to strip.
				return exactBody(row.TerminalBytes)
			}
			// A root-private frame is storage evidence, never a result offer.
			// Only its existing signed public certificate can satisfy the old
			// offered body's commitment after an ACK tombstone drops that body.
			public, err := json.Marshal(result)
			return err == nil && exactBody(public)
		}
	case ScopedDeliveryCoverage:
		for _, row := range g.coverageRows {
			if row.OperationUID == header.ObjectUID && exactBody(row.CertificateBytes) {
				return true
			}
		}
	case ScopedDeliveryControl:
		row := g.controls[header.ObjectUID+":"+header.BindingUID]
		if row != nil {
			return exactBody(row.RequestBytes) || row.ResultBytes != nil && exactBody(row.ResultBytes)
		}
	case ScopedDeliveryConversion:
		if row := g.cutovers[header.ObjectUID]; row != nil && exactBody(row.CertificateBytes) {
			return true
		}
		if row := g.conversions[header.ObjectUID]; row != nil {
			return exactBody(row.RequestBytes) || row.ResultBytes != nil && exactBody(row.ResultBytes)
		}
	}
	return false
}

func (g *scopedImportGraph) validateNegotiation(records []ImportRecord) error {
	var instance string
	for _, record := range records {
		if meta, ok := record.(*MetaKV); ok && meta.Key == "instance_uid" {
			instance = meta.Value
		}
	}
	for _, record := range records {
		switch r := record.(type) {
		case *FederationBindingExport:
			c := r.RelayConfig
			if c == nil || c.ProtocolVersion != ScopedRelayProtocolVersion {
				continue
			}
			if err := c.Validate(instance); err != nil {
				return err
			}
			project := g.projectIDs[r.ProjectID]
			owner := g.ownership[project]
			if r.Role != string(FederationRoleSpoke) || project != r.HubProjectUID || owner == nil || owner.OwnerViewUID == nil || owner.OwnerGeneration == nil || *owner.OwnerViewUID != c.ScopedView.ViewUID || *owner.OwnerGeneration != c.ScopedView.Generation || owner.ProtocolFloor != ScopedRelayProtocolVersion || owner.RootCutoverUID == nil || *owner.RootCutoverUID != c.RootCutoverUID {
				return errors.New("v2 upstream binding lacks exact project owner/floor/cutover")
			}
			view := g.views[c.ScopedView.ViewUID]
			descriptor, ok := g.descriptors[*c.ScopedView]
			if view == nil || !ok || view.AuthorityUID != c.AuthorityUID || (view.BindingUID == c.BindingUID && (view.Epoch != c.ResetEpoch || view.AccountableActor != r.Actor)) || descriptor.DestinationUID != instance || scopedProjectInView(descriptor, project) != nil {
				return errors.New("v2 upstream differs from scoped authenticated tuple/selection")
			}
			// Only destination-side native upstream negotiation names the final
			// signed hop. Generic retained view rows may index any included hop.
			expectedPath := []string{descriptor.AuthorityUID}
			for _, hop := range descriptor.Route {
				expectedPath = append(expectedPath, hop.InstanceUID)
			}
			terminal := descriptor.Route[len(descriptor.Route)-1]
			if !slices.Equal(c.HubPath, expectedPath) || c.UpstreamInstanceUID != expectedPath[len(expectedPath)-2] || c.BindingUID != terminal.BindingUID || c.ResetEpoch != terminal.Epoch {
				return errors.New("v2 native upstream path/binding/epoch differs from terminal retained descriptor route")
			}
		case *FederationEnrollmentExport:
			if r.RelayProtocolVersion != ScopedRelayProtocolVersion {
				continue
			}
			if err := ValidateFederationEnrollmentRelayShape(r.RelayProtocolVersion, r.RelayBindingUID, r.ParentTokenID, r.ProjectID, r.RelayResetEpoch, r.RelayServeDownstream); err != nil {
				return err
			}
			project := g.projectIDs[*r.ProjectID]
			owner := g.ownership[project]
			if owner == nil || owner.ProtocolFloor != ScopedRelayProtocolVersion || owner.RootCutoverUID == nil {
				return errors.New("v2 downstream enrollment lacks verified project floor")
			}
			matched := false
			for _, view := range g.views {
				if view.BindingUID == *r.RelayBindingUID && (view.Epoch != r.RelayResetEpoch || view.AccountableActor != r.Actor) {
					continue
				}
				// Revoked bindings retain owed context, but gain no serving authority.
				for key, descriptor := range g.descriptors {
					if key.ViewUID == view.ViewUID && descriptor.RootCutoverUID == *owner.RootCutoverUID && scopedProjectInView(descriptor, project) == nil && scopedDescriptorHasHop(descriptor, *r.RelayBindingUID, r.RelayResetEpoch, r.SpokeInstanceUID) {
						matched = true
					}
				}
			}
			if !matched {
				return errors.New("v2 downstream lacks exact retained view/epoch/selected membership")
			}
		}
	}
	return nil
}

func (g *scopedImportGraph) validateQuotas(records []ImportRecord) error {
	usage := newScopedSourceBudgetUsage()
	for uid, row := range g.views {
		usage.PrivateBytes[uid] = 0
		if row.State == "active" || row.State == "prepared" {
			usage.ActiveViews[row.AuthorityUID+"/"+row.BindingUID]++
		}
	}
	for _, record := range records {
		if !IsScopedImportRecord(record) {
			continue
		}
		size := ScopedRetainedBytes(record)
		if err := scopedSourceBudgetAdd(usage.FamilyBytes, record.ImportKind(), size); err != nil {
			return err
		}
		var binding, route, view string
		switch r := record.(type) {
		case *ScopedDeliveryRecord:
			binding = r.BindingUID
		case *ScopedSourceOperationExport:
			binding = g.submissions[r.OperationUID].Operation.Principal.BindingUID
			route = g.submissions[r.OperationUID].Operation.Principal.RouteDigest
		case *ScopedPrivateSourceExport:
			if r.OperationUID != nil {
				p := g.submissions[*r.OperationUID].Operation.Principal
				binding, route = p.BindingUID, p.RouteDigest
			}
		case *ScopedControlCustodyExport:
			var p CustodyPrincipal
			if err := scopedJSON(r.PrincipalBytes, &p); err != nil {
				return err
			}
			binding, route = p.BindingUID, p.RouteDigest
		case *ScopedWorkExport:
			view = r.ViewUID
		}
		if binding != "" {
			if err := scopedSourceBudgetAdd(usage.BindingBytes, binding, size); err != nil {
				return err
			}
		}
		for uid, row := range g.views {
			if uid == view || route != "" && row.RouteDigest == route && row.BindingUID == binding {
				if err := scopedSourceBudgetAdd(usage.PrivateBytes, uid, size); err != nil {
					return err
				}
			}
		}
	}
	for uid := range g.operations {
		usage.neighbors[uid] = nil
		usage.OriginalBytes[uid] = 0
		for _, body := range g.submissions[uid].OriginalBytes {
			if err := scopedSourceBudgetAdd(usage.OriginalBytes, uid, int64(len(body))); err != nil {
				return err
			}
		}
	}
	for _, row := range g.dependencies {
		if row.EstablishingOperationUID != nil {
			a, b := row.OperationUID, *row.EstablishingOperationUID
			usage.neighbors[a] = append(usage.neighbors[a], b)
			usage.neighbors[b] = append(usage.neighbors[b], a)
		}
	}
	return usage.validate()
}

func scopedDescriptorHasHop(v ScopedViewDescriptor, binding string, epoch int64, instance string) bool {
	for _, hop := range v.Route {
		if hop.BindingUID == binding && hop.Epoch == epoch && (instance == "" || hop.InstanceUID == instance) {
			return true
		}
	}
	return false
}

// A native hop proves its own accountable actor even when the retained view's
// indexed tuple is another included hop. Without that native context only the
// exact retained view tuple can preserve custody; the origin manifest is never
// evidence of the receiving account. This grants no runtime authority.
func (g *scopedImportGraph) retainedHopPrincipal(p CustodyPrincipal) bool {
	return g.retainedHopPrincipalInView(p, nil)
}

func (g *scopedImportGraph) retainedHopPrincipalInView(p CustodyPrincipal, exactView *ScopedViewKey) bool {
	for key, descriptor := range g.descriptors {
		if exactView != nil && key != *exactView {
			continue
		}
		view := g.views[key.ViewUID]
		if descriptor.AuthorityUID != p.AuthorityUID || descriptor.RouteDigest != p.RouteDigest || !scopedDescriptorHasHop(descriptor, p.BindingUID, p.Epoch, "") {
			continue
		}
		native, valid, newer := false, true, false
		for _, binding := range g.bindings {
			c := binding.RelayConfig
			if c == nil || c.BindingUID != p.BindingUID {
				continue
			}
			if c.ResetEpoch > p.Epoch {
				newer = true
				continue
			}
			native = true
			if c.ScopedView == nil {
				valid = false
				continue
			}
			expected, ok := g.descriptors[*c.ScopedView]
			sameRoute := ok && expected.RouteDigest == p.RouteDigest
			// Only exact framed custody can bind the child to a native owner
			// prefix. Legacy retained tuples keep their existing exact-route law.
			if !sameRoute && exactView != nil {
				sameRoute = g.retainedNativeOwnerPrefix(descriptor, binding)
			}
			if c.ProtocolVersion != ScopedRelayProtocolVersion || c.Validate(g.instance) != nil || !ok || expected.AuthorityUID != p.AuthorityUID || !sameRoute || c.AuthorityUID != p.AuthorityUID || c.ResetEpoch != p.Epoch || binding.Actor != p.AccountableActor || !scopedDescriptorHasHop(expected, p.BindingUID, p.Epoch, "") {
				valid = false
			}
		}
		for _, enrollment := range g.enrollments {
			if enrollment.RelayBindingUID == nil || *enrollment.RelayBindingUID != p.BindingUID {
				continue
			}
			if enrollment.RelayResetEpoch > p.Epoch {
				newer = true
				continue
			}
			native = true
			if enrollment.RelayProtocolVersion != ScopedRelayProtocolVersion || enrollment.RelayResetEpoch != p.Epoch || enrollment.Actor != p.AccountableActor || !scopedDescriptorHasHop(descriptor, p.BindingUID, p.Epoch, enrollment.SpokeInstanceUID) || enrollment.ProjectID == nil || scopedProjectInView(descriptor, g.projectIDs[*enrollment.ProjectID]) != nil {
				valid = false
			}
		}
		if !valid {
			return false
		}
		if native || (!newer || view.State == "retired") && view.BindingUID == p.BindingUID && view.Epoch == p.Epoch && view.AccountableActor == p.AccountableActor {
			return true
		}
	}
	return false
}

func (g *scopedImportGraph) retainedNativeOwnerPrefix(child ScopedViewDescriptor, binding *FederationBindingExport) bool {
	c := binding.RelayConfig
	if c == nil || c.ScopedView == nil {
		return false
	}
	owner, ok := g.descriptors[*c.ScopedView]
	row := g.views[c.ScopedView.ViewUID]
	if !ok || row == nil || row.ActiveGeneration == nil || *row.ActiveGeneration != c.ScopedView.Generation || owner.DestinationUID != g.instance || owner.AuthorityUID != child.AuthorityUID || owner.RootCutoverUID != child.RootCutoverUID || c.RootCutoverUID != child.RootCutoverUID || len(owner.Route) == 0 || len(owner.Route) >= len(child.Route) || !slices.Equal(owner.Route, child.Route[:len(owner.Route)]) {
		return false
	}
	terminal := owner.Route[len(owner.Route)-1]
	path := []string{owner.AuthorityUID}
	for _, hop := range owner.Route {
		path = append(path, hop.InstanceUID)
	}
	if terminal.InstanceUID != g.instance || terminal.BindingUID != c.BindingUID || terminal.Epoch != c.ResetEpoch || !slices.Equal(c.HubPath, path) || c.UpstreamInstanceUID != path[len(path)-2] || row.BindingUID == c.BindingUID && (row.Epoch != c.ResetEpoch || row.AccountableActor != binding.Actor) {
		return false
	}
	for _, selected := range child.Projects {
		if !slices.ContainsFunc(owner.Projects, func(pin ScopedProjectPin) bool { return reflect.DeepEqual(pin, selected) }) {
			return false
		}
		stamp := g.ownership[selected.ProjectUID]
		if stamp == nil || stamp.OwnerViewUID == nil || stamp.OwnerGeneration == nil || *stamp.OwnerViewUID != c.ScopedView.ViewUID || *stamp.OwnerGeneration != c.ScopedView.Generation || stamp.ProtocolFloor != ScopedRelayProtocolVersion || stamp.RootCutoverUID == nil || *stamp.RootCutoverUID != c.RootCutoverUID {
			return false
		}
		matched := false
		for _, peer := range g.bindings {
			if g.projectIDs[peer.ProjectID] != selected.ProjectUID {
				continue
			}
			pc := peer.RelayConfig
			if peer.Role != string(FederationRoleSpoke) || peer.HubProjectUID != selected.ProjectUID || peer.Actor != binding.Actor || pc == nil || pc.Validate(g.instance) != nil || pc.ProtocolVersion != ScopedRelayProtocolVersion || pc.ScopedView == nil || *pc.ScopedView != *c.ScopedView || pc.AuthorityUID != c.AuthorityUID || pc.RootCutoverUID != c.RootCutoverUID || pc.BindingUID != c.BindingUID || pc.ResetEpoch != c.ResetEpoch || pc.UpstreamInstanceUID != c.UpstreamInstanceUID || !slices.Equal(pc.HubPath, c.HubPath) {
				return false
			}
			matched = true
		}
		if !matched {
			return false
		}
	}
	return true
}

func (g *scopedImportGraph) validateRecordInventory(records []ImportRecord) error {
	actual := map[ScopedViewKey]map[string]ScopedEffect{}
	for _, record := range records {
		row, ok := record.(*ScopedRecordExport)
		if !ok {
			continue
		}
		key := ScopedViewKey{row.ViewUID, row.Generation}
		if actual[key] == nil {
			actual[key] = map[string]ScopedEffect{}
		}
		var effect ScopedEffect
		if err := scopedJSON(row.RecordBytes, &effect); err != nil {
			return err
		}
		actual[key][scopedRecordEffectIdentity(effect)] = effect
	}
	for key, generation := range g.generations {
		if generation.CheckpointBytes == nil {
			if len(actual[key]) != 0 {
				return errors.New("projection rows lack authenticated baseline")
			}
			continue
		}
		var checkpoint ScopedCheckpoint
		if err := scopedJSON(generation.CheckpointBytes, &checkpoint); err != nil {
			return err
		}
		batches := []ScopedBatch{}
		for _, row := range g.batches[key] {
			var batch ScopedBatch
			if err := scopedJSON(row.BatchBytes, &batch); err != nil {
				return err
			}
			batches = append(batches, batch)
		}
		inventory, err := ScopedRecordInventory(checkpoint, batches)
		if err != nil {
			return err
		}
		expected := map[string]ScopedEffect{}
		for _, effect := range inventory {
			identity := scopedRecordEffectIdentity(effect)
			expected[identity] = effect
			row, present := actual[key][identity]
			optional := effect.Action == "remove" && effect.Revision <= checkpoint.Cut.Sequence
			if !present && !optional {
				return errors.New("required signed projection row missing")
			}
			if present {
				retained, err := scopedRecordAssertionBytes(row)
				if err != nil {
					return err
				}
				signed, err := scopedRecordAssertionBytes(effect)
				if err != nil {
					return err
				}
				if !bytes.Equal(retained, signed) {
					return errors.New("projection row differs from latest applicable signed assertion")
				}
			}
		}
		for identity := range actual[key] {
			if _, ok := expected[identity]; !ok {
				return errors.New("projection row lacks applicable signed proof")
			}
		}
	}
	return nil
}

// Match the immutable signed route in either transport direction. A forwarded
// downstream push carries its accumulated leaf-to-sender suffix; an upstream
// delivery carries the authority-to-sender prefix. No path is inferred from an
// actor label, and the full retained header remains unchanged.
func (g *scopedImportGraph) viewInboundRoute(view *ScopedViewExport, header ScopedDeliveryHeader) bool {
	if header.ReceiverInstanceUID != g.instance {
		return false
	}
	for key, descriptor := range g.descriptors {
		if key.ViewUID == view.ViewUID && scopedDescriptorDeliveryRoute(descriptor, header) {
			return true
		}
	}
	return false
}

func (g *scopedImportGraph) principalDeliveryRoute(principal CustodyPrincipal, header ScopedDeliveryHeader) bool {
	for _, descriptor := range g.descriptors {
		if descriptor.AuthorityUID == principal.AuthorityUID && descriptor.RouteDigest == principal.RouteDigest && scopedDescriptorDeliveryRoute(descriptor, header) {
			return true
		}
	}
	return false
}

// This matches retained signed routing data, not current grant or actor
// authority. Native admission still owns live permission and final fences.
func scopedDescriptorDeliveryRoute(descriptor ScopedViewDescriptor, header ScopedDeliveryHeader) bool {
	if descriptor.AuthorityUID != header.AuthorityUID {
		return false
	}
	path := []string{descriptor.AuthorityUID}
	for _, hop := range descriptor.Route {
		path = append(path, hop.InstanceUID)
	}
	for i, hop := range descriptor.Route {
		if hop.BindingUID != header.BindingUID || hop.Epoch != header.Epoch {
			continue
		}
		if header.SenderInstanceUID == path[i] && header.ReceiverInstanceUID == path[i+1] && slices.Equal(header.Path, path[:i+1]) {
			return true
		}
		if header.SenderInstanceUID == path[i+1] && header.ReceiverInstanceUID == path[i] {
			suffix := slices.Clone(path[i+1:])
			slices.Reverse(suffix)
			if slices.Equal(header.Path, suffix) && ValidateRelayDownstreamPath(header.AuthorityUID, nil, header.Path) == nil {
				return true
			}
		}
	}
	return false
}

// A surviving native row vetoes retained-only recovery of its current stream;
// fallback must never override its account/path or leaf-forwarding policy.
// A newer epoch is separate; retainedHopPrincipal then requires an exact
// independently authenticated retired tuple for the historical epoch.
func (g *scopedImportGraph) nativeScopedHopPresent(binding string, epoch int64) bool {
	for _, row := range g.bindings {
		if row.RelayConfig != nil && row.RelayConfig.BindingUID == binding && row.RelayConfig.ResetEpoch <= epoch {
			return true
		}
	}
	for _, row := range g.enrollments {
		if row.RelayBindingUID != nil && *row.RelayBindingUID == binding && row.RelayResetEpoch <= epoch {
			return true
		}
	}
	return false
}
