package db

// Typed owner-backup rows. Bytes are retained verbatim; decoding validates a
// copy and never replaces the retained authority with re-encoded JSON.
type ScopedViewExport struct {
	ViewUID            string `json:"view_uid"`
	CatalogEntryUID    string `json:"catalog_entry_uid"`
	AuthorityUID       string `json:"authority_uid"`
	BindingUID         string `json:"binding_uid"`
	Epoch              int64  `json:"epoch"`
	AccountableActor   string `json:"accountable_actor"`
	RouteDigest        string `json:"route_digest"`
	DestinationUID     string `json:"destination_uid"`
	ActiveGeneration   *int64 `json:"active_generation"`
	PreparedGeneration *int64 `json:"prepared_generation"`
	State              string `json:"state"`
	ReasonCode         string `json:"reason_code"`
	RequestDigest      string `json:"request_digest"`
}

func (*ScopedViewExport) ImportKind() string { return "federation_scoped_view" }

type ScopedGenerationExport struct {
	ViewUID          string              `json:"view_uid"`
	Generation       int64               `json:"generation"`
	DescriptorBytes  []byte              `json:"descriptor_bytes"`
	DescriptorDigest string              `json:"descriptor_digest"`
	CheckpointBytes  ScopedNullableBytes `json:"checkpoint_bytes"`
	CheckpointDigest *string             `json:"checkpoint_digest"`
	CutSequence      int64               `json:"cut_sequence"`
	CutDigest        string              `json:"cut_digest"`
	State            string              `json:"state"`
	AssembledBytes   int64               `json:"assembled_bytes"`
	RootCutoverUID   string              `json:"root_cutover_uid"`
}

func (*ScopedGenerationExport) ImportKind() string { return "federation_scoped_generation" }

type ScopedProjectExport struct {
	ProjectUID      string  `json:"project_uid"`
	OwnerViewUID    *string `json:"owner_view_uid"`
	OwnerGeneration *int64  `json:"owner_generation"`
	ProtocolFloor   int64   `json:"protocol_floor"`
	RootCutoverUID  *string `json:"root_cutover_uid"`
	ConversionUID   *string `json:"conversion_uid"`
	ServingFenced   bool    `json:"serving_fenced"`
}

func (*ScopedProjectExport) ImportKind() string { return "federation_scoped_project" }

type ScopedRecordExport struct {
	ViewUID         string  `json:"view_uid"`
	Generation      int64   `json:"generation"`
	Collection      string  `json:"collection"`
	RecordKey       string  `json:"record_key"`
	ProjectUID      string  `json:"project_uid"`
	SubjectUID      *string `json:"subject_uid"`
	PeerProjectUID  *string `json:"peer_project_uid"`
	PeerUID         *string `json:"peer_uid"`
	VisibleRevision int64   `json:"visible_revision"`
	Present         bool    `json:"present"`
	RecordBytes     []byte  `json:"record_bytes"`
	Digest          string  `json:"digest"`
}

func (*ScopedRecordExport) ImportKind() string { return "federation_scoped_record" }

type ScopedBatchExport struct {
	ViewUID        string `json:"view_uid"`
	Generation     int64  `json:"generation"`
	Sequence       int64  `json:"sequence"`
	Digest         string `json:"digest"`
	PreviousDigest string `json:"previous_digest"`
	CaptureUID     string `json:"capture_uid"`
	BatchBytes     []byte `json:"batch_bytes"`
}

func (*ScopedBatchExport) ImportKind() string { return "federation_scoped_batch" }

type ScopedWorkExport struct {
	ViewUID          string              `json:"view_uid"`
	DirtyRevision    int64               `json:"dirty_revision"`
	CapturedRevision *int64              `json:"captured_revision"`
	CaptureUID       *string             `json:"capture_uid"`
	CaptureBytes     ScopedNullableBytes `json:"capture_bytes"`
	CaptureDigest    *string             `json:"capture_digest"`
	State            string              `json:"state"`
}

func (*ScopedWorkExport) ImportKind() string { return "federation_scoped_work" }

func (*ScopedDeliveryRecord) ImportKind() string { return "federation_scoped_delivery" }

func (*ScopedDeliveryCursor) ImportKind() string { return "federation_scoped_cursor" }

type ScopedSourceOperationExport struct {
	OperationUID       string              `json:"operation_uid"`
	Digest             string              `json:"digest"`
	SourceInstanceUID  string              `json:"source_instance_uid"`
	CommandKind        string              `json:"command_kind"`
	ManifestBytes      []byte              `json:"manifest_bytes"`
	PrincipalBytes     []byte              `json:"principal_bytes"`
	PrincipalKey       string              `json:"principal_key"`
	State              string              `json:"state"`
	OverlayState       string              `json:"overlay_state"`
	AdmissionBytes     ScopedNullableBytes `json:"admission_bytes"`
	TerminalBytes      ScopedNullableBytes `json:"terminal_bytes"`
	CancelRequested    bool                `json:"cancel_requested"`
	IdempotencyKey     *string             `json:"idempotency_key"`
	RequestFingerprint string              `json:"request_fingerprint"`
}

func (*ScopedSourceOperationExport) ImportKind() string { return "federation_source_operation" }

type ScopedPrivateSourceExport struct {
	EventUID        string              `json:"event_uid"`
	ContentHash     string              `json:"content_hash"`
	ProjectUID      string              `json:"project_uid"`
	OperationUID    *string             `json:"operation_uid"`
	Ordinal         *int64              `json:"ordinal"`
	ConversionUID   *string             `json:"conversion_uid"`
	EventBytes      []byte              `json:"event_bytes"`
	ProvenanceBytes ScopedNullableBytes `json:"provenance_bytes"`
}

func (*ScopedPrivateSourceExport) ImportKind() string { return "federation_private_source" }

type ScopedSourceCustodyExport struct {
	OperationUID       string `json:"operation_uid"`
	BindingUID         string `json:"binding_uid"`
	Epoch              int64  `json:"epoch"`
	Direction          string `json:"direction"`
	PrincipalBytes     []byte `json:"principal_bytes"`
	ManifestDigest     string `json:"manifest_digest"`
	OwnsComplete       bool   `json:"owns_complete"`
	ResultAcknowledged bool   `json:"result_acknowledged"`
	InvalidationState  string `json:"invalidation_state"`
}

func (*ScopedSourceCustodyExport) ImportKind() string { return "federation_source_custody" }

type ScopedSourceDependencyExport struct {
	OperationUID             string  `json:"operation_uid"`
	ProjectUID               string  `json:"project_uid"`
	IssueUID                 string  `json:"issue_uid"`
	EstablishingOperationUID *string `json:"establishing_operation_uid"`
	ProofBytes               []byte  `json:"proof_bytes"`
	State                    string  `json:"state"`
	TerminalReason           *string `json:"terminal_reason"`
}

func (*ScopedSourceDependencyExport) ImportKind() string { return "federation_source_dependency" }

type ScopedSourceCoverageExport struct {
	OperationUID     string `json:"operation_uid"`
	ViewUID          string `json:"view_uid"`
	Generation       int64  `json:"generation"`
	CutDigest        string `json:"cut_digest"`
	Outcome          string `json:"outcome"`
	CertificateBytes []byte `json:"certificate_bytes"`
}

func (*ScopedSourceCoverageExport) ImportKind() string { return "federation_source_coverage" }

type ScopedControlCustodyExport struct {
	RequestUID         string              `json:"request_uid"`
	BindingUID         string              `json:"binding_uid"`
	RequestDigest      string              `json:"request_digest"`
	PrincipalBytes     []byte              `json:"principal_bytes"`
	RequestBytes       []byte              `json:"request_bytes"`
	ResultBytes        ScopedNullableBytes `json:"result_bytes"`
	State              string              `json:"state"`
	ResultAcknowledged bool                `json:"result_acknowledged"`
	ConversionUID      *string             `json:"conversion_uid"`
}

func (*ScopedControlCustodyExport) ImportKind() string { return "federation_control_custody" }

type ScopedMutationExpectationExport struct {
	TokenDigest          string  `json:"token_digest"`
	ProjectUID           string  `json:"project_uid"`
	SubjectUID           string  `json:"subject_uid"`
	PredicateKind        string  `json:"predicate_kind"`
	PredicateBytes       []byte  `json:"predicate_bytes"`
	PrincipalBytes       []byte  `json:"principal_bytes"`
	RouteDigest          string  `json:"route_digest"`
	Nonce                string  `json:"nonce"`
	ExpiresAt            string  `json:"expires_at"`
	ConsumedOperationUID *string `json:"consumed_operation_uid"`
}

func (*ScopedMutationExpectationExport) ImportKind() string { return "federation_mutation_expectation" }

type ScopedPredicateIncarnationExport struct {
	ProjectUID      string `json:"project_uid"`
	SubjectUID      string `json:"subject_uid"`
	PredicateKind   string `json:"predicate_kind"`
	Incarnation     int64  `json:"incarnation"`
	CanonicalDigest string `json:"canonical_digest"`
}

func (*ScopedPredicateIncarnationExport) ImportKind() string {
	return "federation_predicate_incarnation"
}

type ScopedCanonicalCloseExport struct {
	IssueUID         string              `json:"issue_uid"`
	ProjectUID       string              `json:"project_uid"`
	StatusIntentUID  *string             `json:"status_intent_uid"`
	StatusClockBytes []byte              `json:"status_clock_bytes"`
	ValueBytes       ScopedNullableBytes `json:"value_bytes"`
}

func (*ScopedCanonicalCloseExport) ImportKind() string { return "federation_canonical_close_state" }

type ScopedProtocolCutoverExport struct {
	CutoverUID             string `json:"cutover_uid"`
	RequestDigest          string `json:"request_digest"`
	ProjectUIDsBytes       []byte `json:"project_uids_bytes"`
	Floor                  int64  `json:"floor"`
	ReadinessBytes         []byte `json:"readiness_bytes"`
	SuspendedBindingsBytes []byte `json:"suspended_bindings_bytes"`
	CertificateBytes       []byte `json:"certificate_bytes"`
	State                  string `json:"state"`
}

func (*ScopedProtocolCutoverExport) ImportKind() string { return "federation_protocol_cutover" }

type ScopedConversionExport struct {
	ConversionUID    string              `json:"conversion_uid"`
	RequestDigest    string              `json:"request_digest"`
	RequestBytes     []byte              `json:"request_bytes"`
	OldBindingsBytes []byte              `json:"old_bindings_bytes"`
	RootCutoverUID   *string             `json:"root_cutover_uid"`
	InventoryDigest  *string             `json:"inventory_digest"`
	BaselineDigest   *string             `json:"baseline_digest"`
	Phase            string              `json:"phase"`
	ResultBytes      ScopedNullableBytes `json:"result_bytes"`
}

func (*ScopedConversionExport) ImportKind() string { return "federation_scoped_conversion" }

type ScopedConversionObligationExport struct {
	ConversionUID string `json:"conversion_uid"`
	ObligationUID string `json:"obligation_uid"`
	Kind          string `json:"kind"`
	BindingUID    string `json:"binding_uid"`
	ObjectUID     string `json:"object_uid"`
	Digest        string `json:"digest"`
	Epoch         int64  `json:"epoch"`
	Sequence      int64  `json:"sequence"`
	PrincipalKey  string `json:"principal_key"`
	State         string `json:"state"`
}

func (*ScopedConversionObligationExport) ImportKind() string {
	return "federation_conversion_obligation"
}

// IsScopedImportRecord recognizes only the closed persisted union.
func IsScopedImportRecord(record ImportRecord) bool {
	switch record.(type) {
	case *ScopedViewExport, *ScopedGenerationExport, *ScopedProjectExport, *ScopedRecordExport, *ScopedBatchExport, *ScopedWorkExport, *ScopedDeliveryRecord, *ScopedDeliveryCursor, *ScopedSourceOperationExport, *ScopedPrivateSourceExport, *ScopedSourceCustodyExport, *ScopedSourceDependencyExport, *ScopedSourceCoverageExport, *ScopedControlCustodyExport, *ScopedMutationExpectationExport, *ScopedPredicateIncarnationExport, *ScopedCanonicalCloseExport, *ScopedProtocolCutoverExport, *ScopedConversionExport, *ScopedConversionObligationExport:
		return true
	}
	return false
}

// ScopedImportRank follows dependencies after the existing native kinds.
func ScopedImportRank(kind string) int {
	switch kind {
	case "federation_scoped_view":
		return 40
	case "federation_scoped_generation":
		return 41
	case "federation_scoped_project":
		return 42
	case "federation_canonical_close_state":
		return 43
	case "federation_predicate_incarnation":
		return 44
	case "federation_mutation_expectation":
		return 45
	case "federation_protocol_cutover":
		return 46
	case "federation_scoped_conversion":
		return 47
	case "federation_source_operation":
		return 48
	case "federation_private_source":
		return 49
	case "federation_source_dependency":
		return 50
	case "federation_source_custody":
		return 51
	case "federation_source_coverage":
		return 52
	case "federation_control_custody":
		return 53
	case "federation_conversion_obligation":
		return 54
	case "federation_scoped_work":
		return 55
	case "federation_scoped_record":
		return 56
	case "federation_scoped_batch":
		return 57
	case "federation_scoped_delivery":
		return 58
	case "federation_scoped_cursor":
		return 59
	}
	return -1
}

// ScopedRetainedBytes counts exact retained values, including immutable tombstone
// identity. It is shared admission accounting, never an eviction permission.
func ScopedRetainedBytes(record ImportRecord) int64 {
	var size int64
	switch r := record.(type) {
	case *ScopedViewExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ViewUID))
		size += int64(len(r.CatalogEntryUID))
		size += int64(len(r.AuthorityUID))
		size += int64(len(r.BindingUID))
		size += 8
		size += int64(len(r.AccountableActor))
		size += int64(len(r.RouteDigest))
		size += int64(len(r.DestinationUID))
		if r.ActiveGeneration != nil {
			size += 8
		}
		if r.PreparedGeneration != nil {
			size += 8
		}
		size += int64(len(r.State))
		size += int64(len(r.ReasonCode))
		size += int64(len(r.RequestDigest))
	case *ScopedGenerationExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ViewUID))
		size += 8
		size += int64(len(r.DescriptorBytes))
		size += int64(len(r.DescriptorDigest))
		size += int64(len(r.CheckpointBytes))
		if r.CheckpointDigest != nil {
			size += int64(len(*r.CheckpointDigest))
		}
		size += 8
		size += int64(len(r.CutDigest))
		size += int64(len(r.State))
		size += 8
		size += int64(len(r.RootCutoverUID))
	case *ScopedProjectExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ProjectUID))
		if r.OwnerViewUID != nil {
			size += int64(len(*r.OwnerViewUID))
		}
		if r.OwnerGeneration != nil {
			size += 8
		}
		size += 8
		if r.RootCutoverUID != nil {
			size += int64(len(*r.RootCutoverUID))
		}
		if r.ConversionUID != nil {
			size += int64(len(*r.ConversionUID))
		}
		size++
	case *ScopedRecordExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ViewUID))
		size += 8
		size += int64(len(r.Collection))
		size += int64(len(r.RecordKey))
		size += int64(len(r.ProjectUID))
		if r.SubjectUID != nil {
			size += int64(len(*r.SubjectUID))
		}
		if r.PeerProjectUID != nil {
			size += int64(len(*r.PeerProjectUID))
		}
		if r.PeerUID != nil {
			size += int64(len(*r.PeerUID))
		}
		size += 8
		size++
		size += int64(len(r.RecordBytes))
		size += int64(len(r.Digest))
	case *ScopedBatchExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ViewUID))
		size += 8
		size += 8
		size += int64(len(r.Digest))
		size += int64(len(r.PreviousDigest))
		size += int64(len(r.CaptureUID))
		size += int64(len(r.BatchBytes))
	case *ScopedWorkExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ViewUID))
		size += 8
		if r.CapturedRevision != nil {
			size += 8
		}
		if r.CaptureUID != nil {
			size += int64(len(*r.CaptureUID))
		}
		size += int64(len(r.CaptureBytes))
		if r.CaptureDigest != nil {
			size += int64(len(*r.CaptureDigest))
		}
		size += int64(len(r.State))
	case *ScopedDeliveryRecord:
		if r == nil {
			return 0
		}
		size += int64(len(r.BindingUID))
		size += 8
		size += int64(len(r.Direction))
		size += int64(len(r.Channel))
		size += 8
		size += int64(len(r.ObjectUID))
		size += int64(len(r.ObjectDigest))
		size += int64(len(r.OfferedDigest))
		size += int64(len(r.HeaderBytes))
		size += int64(len(r.EnvelopeBytes))
		size++
		size++
		size++
		if r.Upstream != nil {
			size += int64(len(r.Upstream.BindingUID)+len(r.Upstream.Direction)+len(r.Upstream.Channel)) + 16
		}
	case *ScopedDeliveryCursor:
		if r == nil {
			return 0
		}
		size += int64(len(r.BindingUID))
		size += 8
		size += int64(len(r.Direction))
		size += int64(len(r.Channel))
		size += 8
		size += int64(len(r.ReceivedDigest))
		size += 8
		size += int64(len(r.AcceptedDigest))
		size += 8
		size += int64(len(r.EmittedDigest))
		size += 8
		size += int64(len(r.AcknowledgedDigest))
	case *ScopedSourceOperationExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.OperationUID))
		size += int64(len(r.Digest))
		size += int64(len(r.SourceInstanceUID))
		size += int64(len(r.CommandKind))
		size += int64(len(r.ManifestBytes))
		size += int64(len(r.PrincipalBytes))
		size += int64(len(r.PrincipalKey))
		size += int64(len(r.State))
		size += int64(len(r.OverlayState))
		size += int64(len(r.AdmissionBytes))
		size += int64(len(r.TerminalBytes))
		size++
		if r.IdempotencyKey != nil {
			size += int64(len(*r.IdempotencyKey))
		}
		size += int64(len(r.RequestFingerprint))
	case *ScopedPrivateSourceExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.EventUID))
		size += int64(len(r.ContentHash))
		size += int64(len(r.ProjectUID))
		if r.OperationUID != nil {
			size += int64(len(*r.OperationUID))
		}
		if r.Ordinal != nil {
			size += 8
		}
		if r.ConversionUID != nil {
			size += int64(len(*r.ConversionUID))
		}
		size += int64(len(r.EventBytes))
		size += int64(len(r.ProvenanceBytes))
	case *ScopedSourceCustodyExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.OperationUID))
		size += int64(len(r.BindingUID))
		size += 8
		size += int64(len(r.Direction))
		size += int64(len(r.PrincipalBytes))
		size += int64(len(r.ManifestDigest))
		size++
		size++
		size += int64(len(r.InvalidationState))
	case *ScopedSourceDependencyExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.OperationUID))
		size += int64(len(r.ProjectUID))
		size += int64(len(r.IssueUID))
		if r.EstablishingOperationUID != nil {
			size += int64(len(*r.EstablishingOperationUID))
		}
		size += int64(len(r.ProofBytes))
		size += int64(len(r.State))
		if r.TerminalReason != nil {
			size += int64(len(*r.TerminalReason))
		}
	case *ScopedSourceCoverageExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.OperationUID))
		size += int64(len(r.ViewUID))
		size += 8
		size += int64(len(r.CutDigest))
		size += int64(len(r.Outcome))
		size += int64(len(r.CertificateBytes))
	case *ScopedControlCustodyExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.RequestUID))
		size += int64(len(r.BindingUID))
		size += int64(len(r.RequestDigest))
		size += int64(len(r.PrincipalBytes))
		size += int64(len(r.RequestBytes))
		size += int64(len(r.ResultBytes))
		size += int64(len(r.State))
		size++
		if r.ConversionUID != nil {
			size += int64(len(*r.ConversionUID))
		}
	case *ScopedMutationExpectationExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.TokenDigest))
		size += int64(len(r.ProjectUID))
		size += int64(len(r.SubjectUID))
		size += int64(len(r.PredicateKind))
		size += int64(len(r.PredicateBytes))
		size += int64(len(r.PrincipalBytes))
		size += int64(len(r.RouteDigest))
		size += int64(len(r.Nonce))
		size += int64(len(r.ExpiresAt))
		if r.ConsumedOperationUID != nil {
			size += int64(len(*r.ConsumedOperationUID))
		}
	case *ScopedPredicateIncarnationExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ProjectUID))
		size += int64(len(r.SubjectUID))
		size += int64(len(r.PredicateKind))
		size += 8
		size += int64(len(r.CanonicalDigest))
	case *ScopedCanonicalCloseExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.IssueUID))
		size += int64(len(r.ProjectUID))
		if r.StatusIntentUID != nil {
			size += int64(len(*r.StatusIntentUID))
		}
		size += int64(len(r.StatusClockBytes))
		size += int64(len(r.ValueBytes))
	case *ScopedProtocolCutoverExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.CutoverUID))
		size += int64(len(r.RequestDigest))
		size += int64(len(r.ProjectUIDsBytes))
		size += 8
		size += int64(len(r.ReadinessBytes))
		size += int64(len(r.SuspendedBindingsBytes))
		size += int64(len(r.CertificateBytes))
		size += int64(len(r.State))
	case *ScopedConversionExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ConversionUID))
		size += int64(len(r.RequestDigest))
		size += int64(len(r.RequestBytes))
		size += int64(len(r.OldBindingsBytes))
		if r.RootCutoverUID != nil {
			size += int64(len(*r.RootCutoverUID))
		}
		if r.InventoryDigest != nil {
			size += int64(len(*r.InventoryDigest))
		}
		if r.BaselineDigest != nil {
			size += int64(len(*r.BaselineDigest))
		}
		size += int64(len(r.Phase))
		size += int64(len(r.ResultBytes))
	case *ScopedConversionObligationExport:
		if r == nil {
			return 0
		}
		size += int64(len(r.ConversionUID))
		size += int64(len(r.ObligationUID))
		size += int64(len(r.Kind))
		size += int64(len(r.BindingUID))
		size += int64(len(r.ObjectUID))
		size += int64(len(r.Digest))
		size += 8
		size += 8
		size += int64(len(r.PrincipalKey))
		size += int64(len(r.State))
	}
	return size
}
