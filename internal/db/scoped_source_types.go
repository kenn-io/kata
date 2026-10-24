package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/kata/internal/uid"
	"strings"
)

// Source bodies, results, coverage and custody are private to the authenticated delivery path.
// SourceStatus deliberately carries no coverage certificate.
type SourceMember struct {
	ProjectUID  string `json:"project_uid"`
	EventUID    string `json:"event_uid"`
	ContentHash string `json:"content_hash"`
	Ordinal     int    `json:"ordinal"`
}

type MutationExpectation struct {
	Token string `json:"token"`
}

// CustodyPrincipal is bound to the authenticated native relay enrollment
// BindingUID/RelayResetEpoch. Equal actor strings across routes confer no
// identity. Replacement-token recovery needs explicit authorization for the
// same authority and accountable actor; no token IDs or hashes cross the wire.
type CustodyPrincipal struct {
	Epoch            int64  `json:"epoch"`
	BindingUID       string `json:"binding_uid"`
	AuthorityUID     string `json:"authority_uid"`
	AccountableActor string `json:"accountable_actor"`
	RouteDigest      string `json:"route_digest"`
}

type ScopedMembershipProof struct {
	Endpoint             ScopedEndpoint    `json:"endpoint"`
	Kind                 string            `json:"kind"`
	CreatingOperationUID string            `json:"creating_operation_uid"`
	Signatures           []ScopedSignature `json:"signatures"`
}

type SourceOperation struct {
	Version           int                     `json:"version"`
	UID               string                  `json:"uid"`
	Digest            string                  `json:"digest"`
	SourceInstanceUID string                  `json:"source_instance_uid"`
	CommandKind       string                  `json:"command_kind"`
	IdempotencyKey    string                  `json:"idempotency_key"`
	Principal         CustodyPrincipal        `json:"principal"`
	Members           []SourceMember          `json:"members"`
	Membership        []ScopedMembershipProof `json:"membership"`
	Expectations      []MutationExpectation   `json:"expectations"`
	OwnerGuard        *SourceOwnerGuard       `json:"owner_guard,omitempty"`
	ImportIntent      *ScopedImportIntent     `json:"import_intent,omitempty"`
}

// OriginalBytes retains existing EncodeRelaySourceEvent bytes. WireBytes retains
// the authenticated whole submission for byte-identical forwarding/backup.
// Originals is only the typed decode view, never a direct JSON array of events.
type SourceSubmission struct {
	OriginalBytes [][]byte        `json:"-"`
	WireBytes     []byte          `json:"-"`
	Operation     SourceOperation `json:"operation"`
	Originals     []RemoteEvent   `json:"originals"`
}

type SourceResult struct {
	OperationUID    string               `json:"operation_uid"`
	OperationDigest string               `json:"operation_digest"`
	State           string               `json:"state"`
	ReasonCode      string               `json:"reason_code"`
	Members         []SourceMember       `json:"members"`
	Receipts        []AttributionReceipt `json:"receipts"`
	Signatures      []ScopedSignature    `json:"signatures"`
	ImportOutcome   *ScopedImportOutcome `json:"import_outcome,omitempty"`
}

type SourceCoverage struct {
	OperationUID    string            `json:"operation_uid"`
	OperationDigest string            `json:"operation_digest"`
	View            ScopedViewKey     `json:"view"`
	Cut             ScopedCut         `json:"cut"`
	Outcome         string            `json:"outcome"`
	Signatures      []ScopedSignature `json:"signatures"`
}

type SourceStatus struct {
	OperationUID string        `json:"operation_uid"`
	State        string        `json:"state"`
	Settlement   string        `json:"settlement"`
	Result       *SourceResult `json:"result"`
}

type ScopedMutationSubject struct {
	Kind          string `json:"kind"`
	ProjectUID    string `json:"project_uid"`
	IssueUID      string `json:"issue_uid"`
	DefinitionUID string `json:"definition_uid"`
}

type ScopedMutationPreparation struct {
	Kind          string                   `json:"kind"`
	Subject       ScopedMutationSubject    `json:"subject"`
	Peers         []ScopedEndpoint         `json:"peers"`
	Predicate     string                   `json:"predicate"`
	MetadataGuard *ScopedMetadataPredicate `json:"metadata_guard"`
}

type ScopedMetadataPredicate struct {
	Key      string   `json:"key"`
	IfValue  JSONBlob `json:"if_value"`
	IfAbsent bool     `json:"if_absent"`
}

// ScopedCommand is IN-PROCESS ONLY. Native parameters contain local row IDs.
// Resolve them to stable UIDs before constructing SourceSubmission or
// ScopedMutationPreparation. Never marshal, sign or transmit this type.
type ScopedCommand struct {
	Import               *ImportBatchParams
	Kind, IdempotencyKey string
	Subject              ScopedEndpoint
	Actor, Teammate      string
	Expectations         []MutationExpectation
	Create               *CreateIssueParams
	Edit                 *EditIssueAtomicParams
	Close                *CloseIssueParams
	Comment              *CreateCommentParams
	EditComment          *EditCommentParams
	Link                 *CreateLinkParams
	ReplaceParent        *ReplaceParentAndEventsParams
	Label                *LabelEventParams
	Metadata             *PatchIssueMetadataIn
	ProjectMetadata      *PatchProjectMetadataIn
	Job                  *PutCronJob
	Workflow             *PutCronWorkflow
	Run                  *ObserveCronRun
	Assignment           *ClaimOwnerParams
	Lifecycle            *ScopedLifecycleCommand
}

// ScopedLifecycleCommand carries existing singleton intent after local resolution.
type ScopedLifecycleCommand struct {
	Owner    *string
	Priority *int64
	Label    string
	Remove   bool
}

type ScopedSourceInput struct {
	Command     ScopedCommand
	Current     FoldProjection
	Projects    []Project
	Clock       EventHLCTimestamp
	InstanceUID string
}

func (ScopedCommand) MarshalJSON() ([]byte, error) {
	return nil, errors.New("ScopedCommand is in-process only; encode stable source/preparation DTOs")
}
func (*ScopedCommand) UnmarshalJSON([]byte) error {
	return errors.New("ScopedCommand is not a wire type")
}

func (m MutationExpectation) Validate() error {
	if m.Token == "" || strings.HasPrefix(m.Token, "rev-") {
		return errors.New("scoped expectation requires an opaque preparation token")
	}
	return nil
}

func (p ScopedMutationPreparation) Validate() error {
	s := p.Subject
	if !uid.Valid(s.ProjectUID) {
		return errors.New("invalid preparation project")
	}
	expectedSubject, expectedPredicate := "", ""
	switch p.Kind {
	case "patchProjectMetadata":
		expectedSubject, expectedPredicate = "project", "metadata"
	case "patchIssueMetadata":
		expectedSubject, expectedPredicate = "issue", "metadata"
	case "closeIssue":
		expectedSubject, expectedPredicate = "issue", "close"
	case "editIssue", "createLink", "deleteLink":
		expectedSubject, expectedPredicate = "issue", "parent"
	case "replaceCronJob", "archiveCronJob", "restoreCronJob":
		expectedSubject, expectedPredicate = "job", "definition"
	case "replaceCronWorkflow", "archiveCronWorkflow", "restoreCronWorkflow":
		expectedSubject, expectedPredicate = "workflow", "definition"
	default:
		return errors.New("unsupported preparation command")
	}
	if s.Kind != expectedSubject || p.Predicate != expectedPredicate {
		return errors.New("preparation subject/predicate mismatch")
	}
	switch s.Kind {
	case "project":
		if s.IssueUID != "" || s.DefinitionUID != "" {
			return errors.New("project preparation contains entity")
		}
	case "issue":
		if !uid.Valid(s.IssueUID) || s.DefinitionUID != "" {
			return errors.New("invalid preparation issue")
		}
	case "job", "workflow":
		if !uid.Valid(s.DefinitionUID) || s.IssueUID != "" {
			return errors.New("invalid preparation definition")
		}
	}
	if p.Predicate == "metadata" {
		if p.MetadataGuard == nil {
			return errors.New("invalid metadata predicate union")
		}
		guard := MetadataPatchGuard{Key: p.MetadataGuard.Key, IfValue: jsontext.Value(p.MetadataGuard.IfValue), IfAbsent: p.MetadataGuard.IfAbsent}
		if err := guard.Validate(); err != nil {
			return err
		}
		if p.MetadataGuard.IfValue != "" && !jsontext.Value(p.MetadataGuard.IfValue).IsValid() {
			return errors.New("invalid metadata predicate JSON")
		}
	} else if p.MetadataGuard != nil {
		return errors.New("unexpected metadata predicate")
	}
	seen := map[ScopedEndpoint]bool{}
	for _, peer := range p.Peers {
		if err := peer.Validate(); err != nil {
			return err
		}
		if seen[peer] {
			return errors.New("duplicate preparation peer")
		}
		seen[peer] = true
	}
	if p.Predicate != "parent" && len(p.Peers) != 0 {
		return errors.New("unexpected preparation peer")
	}
	return nil
}

// SourceOperationDigest commits the complete immutable manifest, with only its
// digest field blanked. Member hashes identify canonical native event content;
// exact original and submission bytes are retained and checked separately.
func SourceOperationDigest(operation SourceOperation) (string, error) {
	operation.Digest = ""
	raw, err := json.Marshal(operation)
	if err != nil {
		return "", err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return "", err
	}
	return ScopedOfferedDigest(canonical), nil
}

func (p CustodyPrincipal) Validate() error {
	if !uid.Valid(p.BindingUID) || !uid.Valid(p.AuthorityUID) || ValidateTokenActor(p.AccountableActor) != nil || p.Epoch <= 0 || !scopedDigest(p.RouteDigest) {
		return errors.New("invalid custody principal")
	}
	return nil
}

func (s SourceSubmission) Validate() error {
	bodies, err := s.sourceOriginalBodies()
	if err != nil {
		return err
	}
	return s.validateSourceManifest(bodies)
}

// bodies have already passed the native byte codec in this admission call.
func (s SourceSubmission) validateSourceManifest(bodies [][]byte) error {
	op := s.Operation
	if validateSourceOperationVersion(op) != nil || !uid.Valid(op.UID) || !uid.Valid(op.SourceInstanceUID) || !scopedDigest(op.Digest) || SourceCommandClass(op.CommandKind).Disposition != "source" {
		return errors.New("invalid source operation identity or command")
	}
	if err := op.Principal.Validate(); err != nil {
		return err
	}
	digest, err := SourceOperationDigest(op)
	if err != nil || digest != op.Digest {
		return errors.New("source manifest digest mismatch")
	}
	if op.Version == 3 {
		if len(s.Originals) != 0 || len(bodies) != 0 {
			return errors.New("import intent cannot contain producer originals")
		}
		return nil
	}
	if len(op.Members) == 0 || len(op.Members) != len(s.Originals) {
		return errors.New("invalid source member count")
	}
	for _, expectation := range op.Expectations {
		if err := expectation.Validate(); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	bodyBytes := 0
	for i, member := range op.Members {
		event := s.Originals[i]
		if !uid.Valid(member.ProjectUID) || !uid.Valid(member.EventUID) || !scopedDigest(member.ContentHash) || member.Ordinal != i || seen[member.EventUID] {
			return errors.New("invalid/duplicate source member identity")
		}
		seen[member.EventUID] = true
		if event.EventUID != member.EventUID || event.ProjectUID != member.ProjectUID || event.ContentHash != member.ContentHash || event.OriginInstanceUID != op.SourceInstanceUID {
			return errors.New("source member/body mismatch")
		}
		if _, _, err := FederationEventWireVersion(event.Type); err != nil {
			return err
		}
		// Count exact native wire bodies, including payload whitespace and
		// the existing byte-preserving source wrapper, never compacted JSON.
		bodyBytes += len(bodies[i])
		if bodyBytes > MaxScopedComponentOriginalBytes {
			return errors.New("source component bodies exceed bound")
		}
	}
	membership := map[ScopedEndpoint]bool{}
	for _, proof := range op.Membership {
		if err := proof.Endpoint.Validate(); err != nil {
			return err
		}
		if membership[proof.Endpoint] {
			return errors.New("duplicate membership identity")
		}
		membership[proof.Endpoint] = true
		switch proof.Kind {
		case "root_membership":
			if proof.CreatingOperationUID != "" {
				return errors.New("root membership contains creation assertion")
			}
		case "component_creation":
			if !uid.Valid(proof.CreatingOperationUID) {
				return errors.New("invalid component creation assertion")
			}
		default:
			return errors.New("unknown membership proof kind")
		}
	}
	if op.OwnerGuard != nil {
		event := s.Originals[0]
		if len(s.Originals) != 1 || event.Type != "issue.unassigned" || event.IssueUID == nil || *event.IssueUID != op.OwnerGuard.Subject.IssueUID || event.ProjectUID != op.OwnerGuard.Subject.ProjectUID || event.RelatedIssueUID != nil {
			return errors.New("owner guard differs from sole unassignment original")
		}
	}
	return nil
}

func (c ScopedCommand) Validate() error {
	if SourceCommandClass(c.Kind).Disposition != "source" {
		return errors.New("unsupported scoped source command")
	}
	for _, e := range c.Expectations {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	// Native values are not interchangeable even when their storage method shares
	// code. This closed union makes accidental command-body reuse fail early.
	bodies := []bool{c.Import != nil, c.Create != nil, c.Edit != nil, c.Close != nil, c.Comment != nil, c.EditComment != nil, c.Link != nil, c.ReplaceParent != nil, c.Label != nil, c.Metadata != nil, c.ProjectMetadata != nil, c.Job != nil, c.Workflow != nil, c.Run != nil, c.Assignment != nil, c.Lifecycle != nil}
	count := 0
	for _, present := range bodies {
		if present {
			count++
		}
	}
	if count != 1 {
		return errors.New("scoped command requires exactly one native body")
	}
	match := false
	switch c.Kind {
	case "importIssues":
		match = c.Import != nil
	case "createIssue":
		match = c.Create != nil
	case "editIssue":
		match = c.Edit != nil || c.ReplaceParent != nil
	case "closeIssue":
		match = c.Close != nil
	case "createComment":
		match = c.Comment != nil
	case "editComment":
		match = c.EditComment != nil
	case "createLink", "deleteLink":
		match = c.Link != nil || c.ReplaceParent != nil
	case "addLabel", "removeLabel":
		match = c.Label != nil
	case "patchIssueMetadata":
		match = c.Metadata != nil
	case "patchProjectMetadata":
		match = c.ProjectMetadata != nil
	case "createCronJob", "replaceCronJob", "archiveCronJob", "restoreCronJob":
		match = c.Job != nil
	case "createCronWorkflow", "replaceCronWorkflow", "archiveCronWorkflow", "restoreCronWorkflow":
		match = c.Workflow != nil
	case "observeCronRun":
		match = c.Run != nil
	case "assignIssue", "unassignIssue":
		match = c.Assignment != nil || c.Lifecycle != nil
	case "setIssuePriority", "reopenIssue", "deleteIssue", "restoreIssue":
		match = c.Lifecycle != nil
	}
	if !match {
		return errors.New("scoped command kind/body mismatch")
	}
	if c.Close != nil && c.Close.IfMatchRev != nil || c.Metadata != nil && c.Metadata.IfMatchRev != nil || c.ProjectMetadata != nil && c.ProjectMetadata.IfMatchRev != nil {
		return errors.New("numeric canonical guards unavailable in scoped mode")
	}
	if c.Job != nil && c.Job.ExpectedEventUID != "" || c.Workflow != nil && c.Workflow.ExpectedEventUID != "" {
		return errors.New("canonical cron event guards unavailable in scoped mode")
	}
	return nil
}

func (r SourceResult) Validate() error {
	if !uid.Valid(r.OperationUID) || !scopedDigest(r.OperationDigest) {
		return errors.New("invalid source result identity")
	}
	switch r.State {
	case "accepted", "rejected", "canceled":
	default:
		return errors.New("nonterminal source result")
	}
	if r.State != "accepted" && len(r.Receipts) != 0 {
		return errors.New("unsuccessful result contains receipts")
	}
	if r.ImportOutcome != nil && (!scopedDigest(r.ImportOutcome.Commitment) || len(r.Members) != 0 || len(r.Receipts) != 0) {
		return errors.New("invalid exclusive import result outcome")
	}
	return nil
}

// Validate checks only coverage shape. Trusted signatures, operation binding
// and evidence of the published cut remain the owning consumer's checks.
func (c SourceCoverage) Validate() error {
	if !uid.Valid(c.OperationUID) || !scopedDigest(c.OperationDigest) {
		return errors.New("invalid source coverage identity")
	}
	if err := c.View.Validate(); err != nil {
		return err
	}
	if err := c.Cut.Validate(); err != nil {
		return err
	}
	switch c.Outcome {
	case "represented", "unchanged", "outside_scope":
		return nil
	default:
		return errors.New("unknown source coverage outcome")
	}
}

// ScopedPrincipalKey indexes exact authenticated custody; it is never an
// authorization token or a portable account/grant identity. Live authentication
// must choose the tuple before this commitment is computed.
func ScopedPrincipalKey(principal CustodyPrincipal) (string, error) {
	if err := principal.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Domain    string           `json:"domain"`
		Principal CustodyPrincipal `json:"principal"`
	}{"kata.scoped.principal.v1", principal})
	if err != nil {
		return "", err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return "", err
	}
	return ScopedOfferedDigest(canonical), nil
}

// ScopedSourceSubmissionWire is the HTTP/schema shape. Originals are encoded
// existing relay source bodies, not decoded RemoteEvent JSON documents.
type ScopedSourceSubmissionWire struct {
	Operation SourceOperation `json:"operation"`
	Originals [][]byte        `json:"originals"`
}

func (s SourceSubmission) sourceOriginalBodies() ([][]byte, error) {
	if s.OriginalBytes != nil && len(s.OriginalBytes) != len(s.Originals) {
		return nil, errors.New("source wire/typed member count mismatch")
	}
	bodies := make([][]byte, len(s.Originals))
	for i, event := range s.Originals {
		if s.OriginalBytes == nil {
			encoded, err := EncodeRelaySourceEvent(event)
			if err != nil {
				return nil, err
			}
			bodies[i] = encoded
			continue
		}
		decoded, err := DecodeRelaySourceEvent(s.OriginalBytes[i])
		if err != nil {
			return nil, err
		}
		// The retained body has passed native hash validation. Exact serializer
		// equality transfers that validation to the typed view without hashing
		// the same event again. Fresh, unretained originals still validate above.
		encoded, err := marshalRelaySourceEvent(event)
		if err != nil {
			return nil, err
		}
		comparison, err := marshalRelaySourceEvent(decoded)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(encoded, comparison) {
			return nil, errors.New("retained source bytes disagree with typed original")
		}
		bodies[i] = s.OriginalBytes[i]
	}
	return bodies, nil
}

func (s SourceSubmission) MarshalJSON() ([]byte, error) {
	bodies, err := s.sourceOriginalBodies()
	if err != nil {
		return nil, err
	}
	return s.marshalSourceBodies(bodies)
}

func (s SourceSubmission) marshalSourceBodies(bodies [][]byte) ([]byte, error) {
	wire := ScopedSourceSubmissionWire{Operation: s.Operation, Originals: bodies}
	if s.WireBytes != nil {
		var retained ScopedSourceSubmissionWire
		if err := json.Unmarshal(s.WireBytes, &retained, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		a, err := json.Marshal(wire)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(retained)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(a, b) {
			return nil, errors.New("authenticated submission bytes disagree with typed view")
		}
		return bytes.Clone(s.WireBytes), nil
	}
	return json.Marshal(wire)
}

func (s *SourceSubmission) UnmarshalJSON(raw []byte) error {
	var wire ScopedSourceSubmissionWire
	if err := json.Unmarshal(raw, &wire, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	originals := make([]RemoteEvent, len(wire.Originals))
	for i, body := range wire.Originals {
		event, err := DecodeRelaySourceEvent(body)
		if err != nil {
			return err
		}
		originals[i] = event
	}
	*s = SourceSubmission{Operation: wire.Operation, Originals: originals, OriginalBytes: wire.Originals, WireBytes: bytes.Clone(raw)}
	return nil
}

// EncodeScopedSourceSubmission returns the exact authenticated body, including
// noncanonical outer whitespace, after validating its typed view and retained
// originals. Transport must use these bytes directly as HTTP/envelope bodies.
// Its complete wire bytes must fit the opaque delivery body, not merely the
// larger base64 envelope. json.Marshal is NOT an exact retry codec.
func EncodeScopedSourceSubmission(submission SourceSubmission) ([]byte, error) {
	if len(submission.WireBytes) > MaxScopedCheckpointBytes {
		return nil, errors.New("scoped submission exceeds wire bound")
	}
	bodies, err := submission.sourceOriginalBodies()
	if err != nil {
		return nil, err
	}
	if err := submission.validateSourceManifest(bodies); err != nil {
		return nil, err
	}
	raw, err := submission.marshalSourceBodies(bodies)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxScopedCheckpointBytes {
		return nil, errors.New("scoped submission exceeds wire bound")
	}
	return raw, nil
}

func DecodeScopedSourceSubmission(raw []byte) (SourceSubmission, error) {
	var submission SourceSubmission
	if len(raw) > MaxScopedCheckpointBytes {
		return submission, errors.New("scoped submission exceeds wire bound")
	}
	if err := submission.UnmarshalJSON(raw); err != nil {
		return submission, err
	}
	// UnmarshalJSON decoded and checked each native body into these owned slices.
	if err := submission.validateSourceManifest(submission.OriginalBytes); err != nil {
		return submission, err
	}
	return submission, nil
}
