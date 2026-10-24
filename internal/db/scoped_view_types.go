package db

import (
	"errors"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
	"time"
)

// Scoped records contain current visible facts, never canonical activity or source envelopes.
// CanonicalViewState and CanonicalCloseState are root-private capture inputs.
type ScopedViewKey struct {
	ViewUID    string `json:"view_uid"`
	Generation int64  `json:"generation"`
}

type ScopedEndpoint struct {
	ProjectUID string `json:"project_uid"`
	IssueUID   string `json:"issue_uid"`
}

type ScopedSignature struct {
	ProjectUID string `json:"project_uid"`
	KeyID      string `json:"key_id"`
	Signature  string `json:"signature"`
}

type ScopedProjectPin struct {
	ProjectUID string     `json:"project_uid"`
	Pin        RootKeyPin `json:"pin"`
}

type ScopedHopBinding struct {
	BindingUID  string `json:"binding_uid"`
	InstanceUID string `json:"instance_uid"`
	Epoch       int64  `json:"epoch"`
}

type ScopedViewDescriptor struct {
	Version        int                `json:"version"`
	Schema         int                `json:"schema"`
	Key            ScopedViewKey      `json:"key"`
	AuthorityUID   string             `json:"authority_uid"`
	DestinationUID string             `json:"destination_uid"`
	RouteDigest    string             `json:"route_digest"`
	RequestDigest  string             `json:"request_digest"`
	Projects       []ScopedProjectPin `json:"projects"`
	Route          []ScopedHopBinding `json:"route"`
	RootCutoverUID string             `json:"root_cutover_uid"`
}

type ScopedCut struct {
	Sequence   int64  `json:"sequence"`
	Digest     string `json:"digest"`
	CaptureUID string `json:"capture_uid"`
}

type ScopedCloseValue struct {
	Kind           string `json:"kind"`
	PeerProjectUID string `json:"peer_project_uid"`
	PeerIssueUID   string `json:"peer_issue_uid"`
}

type ScopedReference struct {
	Slot             string            `json:"slot"`
	Kind             string            `json:"kind"`
	Subject          ScopedEndpoint    `json:"subject"`
	SubjectEntityUID string            `json:"subject_entity_uid"`
	Peer             *ScopedEndpoint   `json:"peer"`
	PeerEntityUID    string            `json:"peer_entity_uid"`
	Relation         string            `json:"relation"`
	Close            *ScopedCloseValue `json:"close"`
	Present          bool              `json:"present"`
	Revision         int64             `json:"revision"`
	Signatures       []ScopedSignature `json:"signatures"`
}

type ScopedProvenance struct {
	ProjectUID        string            `json:"project_uid"`
	Kind              string            `json:"kind"`
	EntityUID         string            `json:"entity_uid"`
	Creator           string            `json:"creator"`
	Teammate          string            `json:"teammate"`
	AttributionStatus string            `json:"attribution_status"`
	Signatures        []ScopedSignature `json:"signatures"`
}

type ScopedProjectState struct {
	UID      string   `json:"uid"`
	Name     string   `json:"name"`
	Metadata JSONBlob `json:"metadata"`
}

type ScopedIssueState struct {
	UID                 string     `json:"uid"`
	ProjectUID          string     `json:"project_uid"`
	ShortID             string     `json:"short_id"`
	Title               string     `json:"title"`
	Body                string     `json:"body"`
	Author              string     `json:"author"`
	Status              string     `json:"status"`
	Owner               *string    `json:"owner"`
	Priority            *int64     `json:"priority"`
	AssignmentExpiresOn *time.Time `json:"assignment_expires_on"`
	CreatedAt           time.Time  `json:"created_at"`
	Metadata            JSONBlob   `json:"metadata"`
	Deleted             bool       `json:"deleted"`
}

type ScopedCommentState struct {
	UID        string    `json:"uid"`
	ProjectUID string    `json:"project_uid"`
	IssueUID   string    `json:"issue_uid"`
	Author     string    `json:"author"`
	Teammate   string    `json:"teammate"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

type ScopedLabelState struct {
	ProjectUID string `json:"project_uid"`
	IssueUID   string `json:"issue_uid"`
	Label      string `json:"label"`
}

type ScopedDefinitionState struct {
	UID        string                   `json:"uid"`
	ProjectUID string                   `json:"project_uid"`
	Kind       string                   `json:"kind"`
	Name       string                   `json:"name"`
	Author     string                   `json:"author"`
	CreatedAt  time.Time                `json:"created_at"`
	Deleted    bool                     `json:"deleted"`
	Job        *cron.JobDefinition      `json:"job"`
	Workflow   *cron.WorkflowDefinition `json:"workflow"`
}

type ScopedCollections struct {
	Projects    []ScopedProjectState         `json:"projects"`
	Issues      []ScopedIssueState           `json:"issues"`
	Comments    []ScopedCommentState         `json:"comments"`
	Labels      []ScopedLabelState           `json:"labels"`
	Definitions []ScopedDefinitionState      `json:"definitions"`
	References  []ScopedReference            `json:"references"`
	Provenance  []ScopedProvenance           `json:"provenance"`
	Artifacts   []embedding.ArtifactManifest `json:"artifacts"`
}

type ScopedEffect struct {
	Collection string                      `json:"collection"`
	Key        string                      `json:"key"`
	Action     string                      `json:"action"`
	Revision   int64                       `json:"revision"`
	Project    *ScopedProjectState         `json:"project"`
	Issue      *ScopedIssueState           `json:"issue"`
	Comment    *ScopedCommentState         `json:"comment"`
	Label      *ScopedLabelState           `json:"label"`
	Definition *ScopedDefinitionState      `json:"definition"`
	Reference  *ScopedReference            `json:"reference"`
	Provenance *ScopedProvenance           `json:"provenance"`
	Artifact   *embedding.ArtifactManifest `json:"artifact"`
}

type ScopedComponentDigest struct {
	ProjectUID string `json:"project_uid"`
	Collection string `json:"collection"`
	Digest     string `json:"digest"`
}

type ScopedBatch struct {
	View             ScopedViewDescriptor    `json:"view"`
	Cut              ScopedCut               `json:"cut"`
	PreviousDigest   string                  `json:"previous_digest"`
	Effects          []ScopedEffect          `json:"effects"`
	ComponentDigests []ScopedComponentDigest `json:"component_digests"`
	Signatures       []ScopedSignature       `json:"signatures"`
}

type ScopedCheckpoint struct {
	View        ScopedViewDescriptor `json:"view"`
	Cut         ScopedCut            `json:"cut"`
	Collections ScopedCollections    `json:"collections"`
	KeyHistory  []RootKeyTransition  `json:"key_history"`
	Signatures  []ScopedSignature    `json:"signatures"`
}

type ScopedReadQuery struct {
	View        ScopedViewKey `json:"view"`
	ProjectUIDs []string      `json:"project_uids"`
	IssueUIDs   []string      `json:"issue_uids"`
	Kind        string        `json:"kind"`
	Query       string        `json:"query"`
	Cursor      string        `json:"cursor"`
	Limit       int           `json:"limit"`
}

type ScopedReadResult struct {
	Collections ScopedCollections `json:"collections"`
	ETag        string            `json:"etag"`
	SessionUID  string            `json:"session_uid"`
	Position    int64             `json:"position"`
	NextCursor  string            `json:"next_cursor"`
	Complete    bool              `json:"complete"`
}

type CanonicalCloseState struct {
	IssueUID        string            `json:"issue_uid"`
	StatusIntentUID string            `json:"status_intent_uid"`
	StatusClock     FoldClock         `json:"status_clock"`
	Value           *ScopedCloseValue `json:"value"`
}

type CanonicalViewState struct {
	CaptureUID  string                       `json:"capture_uid"`
	ProjectCuts map[string]int64             `json:"project_cuts"`
	Projects    []Project                    `json:"projects"`
	Fold        FoldProjection               `json:"fold"`
	Creation    []ScopedProvenance           `json:"creation"`
	Close       []CanonicalCloseState        `json:"close"`
	Artifacts   []embedding.ArtifactManifest `json:"artifacts"`
}

func (e ScopedEndpoint) Validate() error {
	if !uid.Valid(e.ProjectUID) || !uid.Valid(e.IssueUID) {
		return errors.New("invalid scoped endpoint")
	}
	return nil
}

func (v ScopedViewKey) Validate() error {
	if !uid.Valid(v.ViewUID) || v.Generation <= 0 {
		return errors.New("invalid scoped view key")
	}
	return nil
}

func (v ScopedViewDescriptor) Validate() error {
	if v.Version != ScopedRelayProtocolVersion || v.Schema != ScopedProjectionSchema {
		return errors.New("unsupported scoped version or schema")
	}
	if err := v.Key.Validate(); err != nil {
		return err
	}
	if !uid.Valid(v.AuthorityUID) || !uid.Valid(v.DestinationUID) || !scopedDigest(v.RouteDigest) || !scopedDigest(v.RequestDigest) || (v.RootCutoverUID != "" && !uid.Valid(v.RootCutoverUID)) {
		return errors.New("invalid scoped view identity")
	}
	if len(v.Projects) == 0 || len(v.Projects) > MaxScopedSelectedProjects || len(v.Route) == 0 || len(v.Route) > MaxRelayPathNodes {
		return errors.New("invalid scoped view scope or route size")
	}
	previous := ""
	for _, p := range v.Projects {
		if p.ProjectUID <= previous || p.Pin.ProjectUID != p.ProjectUID || p.Pin.AuthorityUID != v.AuthorityUID || p.Pin.Retired || ValidateRootKeyPin(p.Pin) != nil {
			return errors.New("invalid or unordered scoped project pin")
		}
		previous = p.ProjectUID
	}
	bindings, instances := map[string]bool{}, map[string]bool{}
	for _, hop := range v.Route {
		if !uid.Valid(hop.BindingUID) || !uid.Valid(hop.InstanceUID) || hop.Epoch <= 0 || bindings[hop.BindingUID] || instances[hop.InstanceUID] {
			return errors.New("invalid or duplicate scoped hop")
		}
		bindings[hop.BindingUID], instances[hop.InstanceUID] = true, true
	}
	if v.Route[len(v.Route)-1].InstanceUID != v.DestinationUID {
		return errors.New("scoped destination is not route endpoint")
	}
	return nil
}

func (r ScopedReference) Validate() error {
	if r.Kind == "definition" {
		if !uid.Valid(r.Subject.ProjectUID) || r.Subject.IssueUID != "" || !uid.Valid(r.SubjectEntityUID) {
			return errors.New("invalid definition-owned reference subject")
		}
	} else if err := r.Subject.Validate(); err != nil {
		return err
	}
	if r.Slot == "" || r.Revision <= 0 {
		return errors.New("invalid reference slot or revision")
	}
	switch r.Kind {
	case "close":
		if r.Peer != nil || r.PeerEntityUID != "" || r.SubjectEntityUID != "" || r.Relation != "" || r.Present != (r.Close != nil) {
			return errors.New("invalid nullable close union")
		}
		if r.Close != nil {
			return r.Close.Validate()
		}
	case "parent", "reply", "link", "definition":
		if r.Close != nil || r.Present != (r.Peer != nil) {
			return errors.New("invalid nullable reference union")
		}
		if r.Kind == "link" && (!r.Present || r.SubjectEntityUID != "" || r.PeerEntityUID != "" || (r.Relation != "blocks" && r.Relation != "related")) {
			return errors.New("invalid link reference")
		}
		if r.Kind == "parent" && (r.SubjectEntityUID != "" || r.PeerEntityUID != "" || r.Relation != "") {
			return errors.New("invalid parent reference")
		}
		if r.Kind == "reply" && (!uid.Valid(r.SubjectEntityUID) || (r.Present && (!uid.Valid(r.PeerEntityUID) || !ValidCommentReplyKind(r.Relation))) || (!r.Present && (r.PeerEntityUID != "" || r.Relation != ""))) {
			return errors.New("invalid reply slot")
		}
		if r.Kind == "definition" && !uid.Valid(r.SubjectEntityUID) {
			return errors.New("invalid definition slot")
		}
		if !r.Present && (r.PeerEntityUID != "" || r.Relation != "") {
			return errors.New("null reference retained peer data")
		}
		if r.Kind == "definition" && r.Peer != nil {
			switch r.Relation {
			case "workflow":
				if !uid.Valid(r.Peer.ProjectUID) || r.Peer.IssueUID != "" || !uid.Valid(r.PeerEntityUID) {
					return errors.New("invalid workflow definition peer")
				}
				return nil
			case "issue":
				if r.PeerEntityUID != "" {
					return errors.New("issue definition peer contains entity alias")
				}
			default:
				return errors.New("unknown definition reference relation")
			}
		}
		if r.Peer != nil {
			return r.Peer.Validate()
		}
	default:
		return errors.New("unknown scoped reference kind")
	}
	return nil
}

func (c ScopedCloseValue) Validate() error {
	if c.Kind != "duplicate" && c.Kind != "superseded" {
		return errors.New("invalid close disposition")
	}
	return (ScopedEndpoint{ProjectUID: c.PeerProjectUID, IssueUID: c.PeerIssueUID}).Validate()
}

func (d ScopedDefinitionState) Validate() error {
	if !uid.Valid(d.UID) || !uid.Valid(d.ProjectUID) {
		return errors.New("invalid definition identity")
	}
	switch d.Kind {
	case "job":
		if d.Job == nil || d.Workflow != nil || d.Job.Version != 1 || d.Job.Kind != "job" {
			return errors.New("invalid job union")
		}
	case "workflow":
		if d.Workflow == nil || d.Job != nil || d.Workflow.Version != 1 {
			return errors.New("invalid workflow union")
		}
	default:
		return errors.New("unknown definition kind")
	}
	return nil
}

func (e ScopedEffect) Validate() error {
	if e.Key == "" || e.Revision <= 0 {
		return errors.New("invalid effect identity")
	}
	bodies := map[string]bool{"projects": e.Project != nil, "issues": e.Issue != nil, "comments": e.Comment != nil, "labels": e.Label != nil, "definitions": e.Definition != nil, "references": e.Reference != nil, "provenance": e.Provenance != nil, "artifacts": e.Artifact != nil}
	matching, known := bodies[e.Collection]
	if !known {
		return errors.New("unknown effect collection")
	}
	count := 0
	for _, present := range bodies {
		if present {
			count++
		}
	}
	if e.Action == "remove" {
		if count != 0 {
			return errors.New("effect removal contains a body")
		}
		return nil
	}
	if e.Action != "replace" || count != 1 || !matching {
		return errors.New("invalid effect replacement union")
	}
	if e.Reference != nil {
		return e.Reference.Validate()
	}
	if e.Definition != nil {
		return e.Definition.Validate()
	}
	return nil
}

// Validate checks authoritative collection completeness and identity uniqueness.
// Membership, root signatures and artifact text eligibility are verified by the
// projector/installer under live authority, not inferred from these shapes.
func (c ScopedCollections) Validate() error {
	if c.Projects == nil || c.Issues == nil || c.Comments == nil || c.Labels == nil || c.Definitions == nil || c.References == nil || c.Provenance == nil || c.Artifacts == nil {
		return errors.New("all scoped collections must be explicit arrays")
	}
	seen := map[string]bool{}
	unique := func(key string) error {
		if seen[key] {
			return errors.New("duplicate scoped collection identity")
		}
		seen[key] = true
		return nil
	}
	for _, p := range c.Projects {
		if !uid.Valid(p.UID) {
			return errors.New("invalid scoped project")
		}
		if err := unique("project:" + p.UID); err != nil {
			return err
		}
	}
	for _, i := range c.Issues {
		if !uid.Valid(i.UID) || !uid.Valid(i.ProjectUID) {
			return errors.New("invalid scoped issue")
		}
		if err := unique("issue:" + i.UID); err != nil {
			return err
		}
	}
	for _, v := range c.Comments {
		if !uid.Valid(v.UID) || !uid.Valid(v.ProjectUID) || !uid.Valid(v.IssueUID) {
			return errors.New("invalid scoped comment")
		}
		if err := unique("comment:" + v.UID); err != nil {
			return err
		}
	}
	for _, v := range c.Labels {
		if !uid.Valid(v.ProjectUID) || !uid.Valid(v.IssueUID) || v.Label == "" {
			return errors.New("invalid scoped label")
		}
		if err := unique("label:" + v.ProjectUID + ":" + v.IssueUID + ":" + v.Label); err != nil {
			return err
		}
	}
	for _, v := range c.Definitions {
		if err := v.Validate(); err != nil {
			return err
		}
		if err := unique("definition:" + v.ProjectUID + ":" + v.UID); err != nil {
			return err
		}
	}
	for _, v := range c.References {
		if err := v.Validate(); err != nil {
			return err
		}
		if err := unique("reference:" + v.Subject.ProjectUID + ":" + v.Subject.IssueUID + ":" + v.SubjectEntityUID + ":" + v.Slot); err != nil {
			return err
		}
	}
	for _, v := range c.Provenance {
		if !uid.Valid(v.ProjectUID) || !uid.Valid(v.EntityUID) || v.Kind == "" {
			return errors.New("invalid scoped provenance")
		}
		if err := unique("provenance:" + v.ProjectUID + ":" + v.Kind + ":" + v.EntityUID); err != nil {
			return err
		}
	}
	for _, v := range c.Artifacts {
		if err := embedding.ValidateArtifactManifest(v); err != nil {
			return err
		}
		if err := unique("artifact:" + v.ProjectUID + ":" + v.Digest); err != nil {
			return err
		}
	}
	return nil
}

func (c ScopedCut) Validate() error {
	if c.Sequence < 0 || !scopedDigest(c.Digest) || !uid.Valid(c.CaptureUID) {
		return errors.New("invalid scoped cut")
	}
	return nil
}
func (b ScopedBatch) Validate() error {
	if err := b.View.Validate(); err != nil {
		return err
	}
	if err := b.Cut.Validate(); err != nil {
		return err
	}
	if b.Cut.Sequence <= 0 || !scopedDigest(b.PreviousDigest) {
		return errors.New("invalid scoped batch chain")
	}
	seen := map[string]bool{}
	for _, effect := range b.Effects {
		if err := effect.Validate(); err != nil {
			return err
		}
		if effect.Revision != b.Cut.Sequence {
			return errors.New("nonatomic scoped batch position")
		}
		key := effect.Collection + ":" + effect.Key
		if seen[key] {
			return errors.New("duplicate batch effect identity")
		}
		seen[key] = true
	}
	previous := ""
	for _, component := range b.ComponentDigests {
		key := component.ProjectUID + ":" + component.Collection
		if !uid.Valid(component.ProjectUID) || component.Collection == "" || !scopedDigest(component.Digest) || key <= previous {
			return errors.New("invalid batch component inventory")
		}
		previous = key
	}
	return nil
}
func (c ScopedCheckpoint) Validate() error {
	if err := c.View.Validate(); err != nil {
		return err
	}
	if err := c.Cut.Validate(); err != nil {
		return err
	}
	return c.Collections.Validate()
}
