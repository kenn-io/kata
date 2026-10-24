package db

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

// ErrScopedCapturePending retains the existing immutable owed cut while a
// different project selection waits. It conveys no public serving capability.
var ErrScopedCapturePending = errors.New("scoped canonical capture remains owed for another project selection")

// The versioned capture is root-private derived current state. It is neither a
// source event nor a public checkpoint, and must never replace retained source
// bytes or their receipts. Its assembled bound does not prove public checkpoint
// capacity or reserve any binding's admission budget.
type scopedCanonicalCapture struct {
	Version     int                          `json:"version"`
	CaptureUID  string                       `json:"capture_uid"`
	ProjectCuts []scopedCanonicalProjectCut  `json:"project_cuts"`
	Projects    []Project                    `json:"projects"`
	Fold        scopedCanonicalFold          `json:"fold"`
	Creation    []ScopedProvenance           `json:"creation"`
	Close       []CanonicalCloseState        `json:"close"`
	Artifacts   []embedding.ArtifactManifest `json:"artifacts"`
}
type scopedCanonicalProjectCut struct {
	ProjectUID string `json:"project_uid"`
	Cut        int64  `json:"cut"`
}
type scopedCanonicalIssue struct {
	CloseBoundaryClock *FoldClock        `json:"close_boundary_clock,omitempty"`
	CloseValue         *ScopedCloseValue `json:"close_value,omitempty"`

	Issue           FoldIssue `json:"issue"`
	StatusIntentUID string    `json:"status_intent_uid"`
	StatusClock     FoldClock `json:"status_clock"`
}
type scopedCanonicalLabel struct {
	Key   FoldLabelKey     `json:"key"`
	State FoldElementState `json:"state"`
}
type scopedCanonicalMetadata struct {
	UID   string         `json:"uid"`
	Value jsontext.Value `json:"value"`
}
type scopedCanonicalFold struct {
	Issues          []scopedCanonicalIssue    `json:"issues"`
	Comments        []FoldComment             `json:"comments"`
	Labels          []scopedCanonicalLabel    `json:"labels"`
	Links           []FoldLinkEdge            `json:"links"`
	IssueMetadata   []scopedCanonicalMetadata `json:"issue_metadata"`
	ProjectMetadata []scopedCanonicalMetadata `json:"project_metadata"`
	CronJobs        []FoldCronJob             `json:"cron_jobs"`
	CronWorkflows   []FoldCronWorkflow        `json:"cron_workflows"`
	CronRuns        []FoldCronRun             `json:"cron_runs"`
	Warnings        []string                  `json:"warnings"`
}

// EncodeScopedCanonicalCapture encodes a bounded, portable current-state
// capture without changing its input. Backend row IDs are zeroed on copies;
// project UID associations, private revisions and source clocks remain intact.
// Native cronAuthors is intentionally omitted: it selects replay candidates,
// while this capture retains only the exported winning author. A decoded capture
// must not be used as an incremental fold accumulator.
func EncodeScopedCanonicalCapture(state CanonicalViewState) ([]byte, error) {
	w := scopedCanonicalCapture{Version: 1, CaptureUID: state.CaptureUID,
		ProjectCuts: make([]scopedCanonicalProjectCut, 0, len(state.ProjectCuts)),
		Projects:    append([]Project{}, state.Projects...), Creation: append([]ScopedProvenance{}, state.Creation...), Close: append([]CanonicalCloseState{}, state.Close...), Artifacts: append([]embedding.ArtifactManifest{}, state.Artifacts...),
		Fold: scopedCanonicalFold{
			Issues: make([]scopedCanonicalIssue, 0, len(state.Fold.Issues)), Comments: make([]FoldComment, 0, len(state.Fold.Comments)), Labels: make([]scopedCanonicalLabel, 0, len(state.Fold.Labels)), Links: make([]FoldLinkEdge, 0, len(state.Fold.Links)),
			IssueMetadata: make([]scopedCanonicalMetadata, 0, len(state.Fold.IssueMetadata)), ProjectMetadata: make([]scopedCanonicalMetadata, 0, len(state.Fold.ProjectMetadata)),
			CronJobs: make([]FoldCronJob, 0, len(state.Fold.CronJobs)), CronWorkflows: make([]FoldCronWorkflow, 0, len(state.Fold.CronWorkflows)), CronRuns: make([]FoldCronRun, 0, len(state.Fold.CronRuns)), Warnings: append([]string{}, state.Fold.Warnings...),
		}}
	for p, cut := range state.ProjectCuts {
		w.ProjectCuts = append(w.ProjectCuts, scopedCanonicalProjectCut{p, cut})
	}
	for i := range w.Projects {
		w.Projects[i].ID = 0
	}
	for k, v := range state.Fold.Issues {
		if k != v.UID {
			return nil, errors.New("capture issue key mismatch")
		}
		w.Fold.Issues = append(w.Fold.Issues, scopedCanonicalIssue{Issue: v, StatusIntentUID: v.StatusIntentUID, StatusClock: v.StatusClock, CloseBoundaryClock: v.CloseBoundaryClock, CloseValue: v.CloseValue})
	}
	for k, v := range state.Fold.Comments {
		if k != v.UID {
			return nil, errors.New("capture comment key mismatch")
		}
		w.Fold.Comments = append(w.Fold.Comments, v)
	}
	for k, v := range state.Fold.Labels {
		w.Fold.Labels = append(w.Fold.Labels, scopedCanonicalLabel{k, v})
	}
	for k, v := range state.Fold.Links {
		w.Fold.Links = append(w.Fold.Links, FoldLinkEdge{Key: k, State: v})
	}
	for k, v := range state.Fold.IssueMetadata {
		w.Fold.IssueMetadata = append(w.Fold.IssueMetadata, scopedCanonicalMetadata{k, v})
	}
	for k, v := range state.Fold.ProjectMetadata {
		w.Fold.ProjectMetadata = append(w.Fold.ProjectMetadata, scopedCanonicalMetadata{k, v})
	}
	for k, v := range state.Fold.CronJobs {
		if k != v.UID {
			return nil, errors.New("capture job key mismatch")
		}
		v.ID = 0
		v.ProjectID = 0
		w.Fold.CronJobs = append(w.Fold.CronJobs, v)
	}
	for k, v := range state.Fold.CronWorkflows {
		if k != v.UID {
			return nil, errors.New("capture workflow key mismatch")
		}
		v.ID = 0
		v.ProjectID = 0
		w.Fold.CronWorkflows = append(w.Fold.CronWorkflows, v)
	}
	for k, v := range state.Fold.CronRuns {
		if k != v.UID {
			return nil, errors.New("capture run key mismatch")
		}
		v.ID = 0
		v.ProjectID = 0
		w.Fold.CronRuns = append(w.Fold.CronRuns, v)
	}
	slices.SortFunc(w.ProjectCuts, func(a, b scopedCanonicalProjectCut) int { return cmp.Compare(a.ProjectUID, b.ProjectUID) })
	slices.SortFunc(w.Projects, func(a, b Project) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.Issues, func(a, b scopedCanonicalIssue) int { return cmp.Compare(a.Issue.UID, b.Issue.UID) })
	slices.SortFunc(w.Fold.Comments, func(a, b FoldComment) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.Labels, func(a, b scopedCanonicalLabel) int {
		if n := cmp.Compare(a.Key.IssueUID, b.Key.IssueUID); n != 0 {
			return n
		}
		return cmp.Compare(a.Key.Label, b.Key.Label)
	})
	slices.SortFunc(w.Fold.Links, func(a, b FoldLinkEdge) int {
		if n := cmp.Compare(a.Key.FromUID, b.Key.FromUID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Key.ToUID, b.Key.ToUID); n != 0 {
			return n
		}
		return cmp.Compare(a.Key.Type, b.Key.Type)
	})
	slices.SortFunc(w.Fold.IssueMetadata, func(a, b scopedCanonicalMetadata) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.ProjectMetadata, func(a, b scopedCanonicalMetadata) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.CronJobs, func(a, b FoldCronJob) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.CronWorkflows, func(a, b FoldCronWorkflow) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Fold.CronRuns, func(a, b FoldCronRun) int { return cmp.Compare(a.UID, b.UID) })
	slices.SortFunc(w.Creation, func(a, b ScopedProvenance) int {
		if n := cmp.Compare(a.ProjectUID, b.ProjectUID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Kind, b.Kind); n != 0 {
			return n
		}
		return cmp.Compare(a.EntityUID, b.EntityUID)
	})
	slices.SortFunc(w.Close, func(a, b CanonicalCloseState) int { return cmp.Compare(a.IssueUID, b.IssueUID) })
	slices.SortFunc(w.Artifacts, func(a, b embedding.ArtifactManifest) int {
		if n := cmp.Compare(a.ProjectUID, b.ProjectUID); n != 0 {
			return n
		}
		return cmp.Compare(a.Digest, b.Digest)
	})
	if _, err := w.state(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxScopedCheckpointBytes {
		return nil, errors.New("private canonical capture exceeds assembled byte limit")
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	if len(canonical) > MaxScopedCheckpointBytes {
		return nil, errors.New("private canonical capture exceeds assembled byte limit")
	}
	return canonical, nil
}

// DecodeScopedCanonicalCapture validates the complete typed capture before
// converting arrays to maps. It performs no fold or database reads and grants
// no source or signed-provenance authority. A store must independently bind the
// capture UID, exact retained-byte digest, revision and project set to its work
// row and authenticated descriptor, and produce the capture at one SQL cut.
func DecodeScopedCanonicalCapture(raw []byte) (CanonicalViewState, error) {
	if len(raw) > MaxScopedCheckpointBytes {
		return CanonicalViewState{}, errors.New("private canonical capture exceeds assembled byte limit")
	}
	canonical := jsontext.Value(bytes.Clone(raw))
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return CanonicalViewState{}, err
	}
	if len(canonical) > MaxScopedCheckpointBytes {
		return CanonicalViewState{}, errors.New("private canonical capture exceeds assembled byte limit")
	}
	var w scopedCanonicalCapture
	if err := json.Unmarshal(canonical, &w, json.RejectUnknownMembers(true)); err != nil {
		return CanonicalViewState{}, err
	}
	// Re-encoding this derived typed object enforces required fields and exact
	// zero/null/array semantics; it never normalizes an original source envelope.
	encoded, err := json.Marshal(w)
	if err != nil {
		return CanonicalViewState{}, err
	}
	typed := jsontext.Value(encoded)
	if err := typed.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return CanonicalViewState{}, err
	}
	if !bytes.Equal(typed, canonical) {
		return CanonicalViewState{}, errors.New("capture is missing required fields or has invalid typed nulls")
	}
	return w.state()
}

func (w scopedCanonicalCapture) state() (CanonicalViewState, error) {
	fail := func(message string) (CanonicalViewState, error) {
		return CanonicalViewState{}, fmt.Errorf("invalid canonical capture: %s", message)
	}
	if w.Version != 1 || !uid.Valid(w.CaptureUID) {
		return fail("version or identity")
	}
	f := w.Fold
	if w.ProjectCuts == nil || w.Projects == nil || w.Creation == nil || w.Close == nil || w.Artifacts == nil || f.Issues == nil || f.Comments == nil || f.Labels == nil || f.Links == nil || f.IssueMetadata == nil || f.ProjectMetadata == nil || f.CronJobs == nil || f.CronWorkflows == nil || f.CronRuns == nil || f.Warnings == nil {
		return fail("collections must be explicit arrays")
	}
	if len(w.Projects) == 0 || len(w.Projects) > MaxScopedSelectedProjects || len(w.ProjectCuts) != len(w.Projects) {
		return fail("project/cut inventory")
	}
	state := CanonicalViewState{CaptureUID: w.CaptureUID, ProjectCuts: map[string]int64{}, Projects: w.Projects, Creation: w.Creation, Close: w.Close, Artifacts: w.Artifacts, Fold: FoldProjection{
		Issues: map[string]FoldIssue{}, Comments: map[string]FoldComment{}, Labels: map[FoldLabelKey]FoldElementState{}, Links: map[FoldLinkKey]FoldElementState{}, IssueMetadata: map[string]jsontext.Value{}, ProjectMetadata: map[string]jsontext.Value{}, CronJobs: map[string]FoldCronJob{}, CronWorkflows: map[string]FoldCronWorkflow{}, CronRuns: map[string]FoldCronRun{}, Warnings: f.Warnings,
	}}
	projects := map[string]bool{}
	for _, v := range w.Projects {
		if !uid.Valid(v.UID) || projects[v.UID] || v.ID != 0 {
			return fail("project identity, duplicate or local row ID")
		}
		if v.Metadata != "" {
			if err := validateCaptureMetadata([]byte(v.Metadata), false); err != nil {
				return fail(err.Error())
			}
		}
		projects[v.UID] = true
	}
	for _, v := range w.ProjectCuts {
		if _, ok := state.ProjectCuts[v.ProjectUID]; ok || !projects[v.ProjectUID] || v.Cut < 0 {
			return fail("cut ownership, duplicate or value")
		}
		state.ProjectCuts[v.ProjectUID] = v.Cut
	}
	for _, v := range f.Issues {
		i := v.Issue
		if _, ok := state.Fold.Issues[i.UID]; ok || !uid.Valid(i.UID) || !projects[i.ProjectUID] || (i.Status != "open" && i.Status != "closed") || (v.StatusIntentUID != "" && !uid.Valid(v.StatusIntentUID)) || !validCaptureClock(v.StatusClock) {
			return fail("issue identity, ownership, status or provenance")
		}
		if v.StatusIntentUID != "" && v.StatusClock == (FoldClock{}) {
			return fail("intent without status clock")
		}
		i.StatusIntentUID = v.StatusIntentUID
		i.StatusClock = v.StatusClock
		i.CloseBoundaryClock = cloneCloseBoundary(v.CloseBoundaryClock)
		i.CloseValue = cloneCloseValue(v.CloseValue)
		if err := ValidateScopedCanonicalCloseState(CanonicalCloseState{IssueUID: i.UID, StatusIntentUID: i.StatusIntentUID, StatusClock: i.StatusClock, CloseBoundaryClock: i.CloseBoundaryClock, Value: i.CloseValue}, i.UID, i.Status); err != nil {
			return fail(err.Error())
		}
		state.Fold.Issues[i.UID] = i
	}
	ownsIssue := func(entity, project string) bool {
		i, ok := state.Fold.Issues[entity]
		return ok && i.ProjectUID == project
	}
	for _, v := range f.Comments {
		if _, ok := state.Fold.Comments[v.UID]; ok || !uid.Valid(v.UID) || !uid.Valid(v.IssueUID) || !validCaptureClock(v.Clock) {
			return fail("comment identity or clock")
		}
		if _, ok := state.Fold.Issues[v.IssueUID]; !ok {
			return fail("comment owner")
		}
		if err := ValidateCommentReply(v.UID, v.ReplyToUID, v.ReplyKind); err != nil || (v.ReplyToUID != "" && !uid.Valid(v.ReplyToUID)) {
			return fail("comment reply")
		}
		state.Fold.Comments[v.UID] = v
	}
	for _, v := range f.Labels {
		if _, ok := state.Fold.Labels[v.Key]; ok || v.Key.Label == "" || !validCaptureClock(v.State.Clock) {
			return fail("label duplicate, key or clock")
		}
		if _, ok := state.Fold.Issues[v.Key.IssueUID]; !ok {
			return fail("label owner")
		}
		state.Fold.Labels[v.Key] = v.State
	}
	for _, v := range f.Links {
		if _, ok := state.Fold.Links[v.Key]; ok || !uid.Valid(v.Key.FromUID) || !uid.Valid(v.Key.ToUID) || v.Key.FromUID == v.Key.ToUID || !validCaptureClock(v.State.Clock) {
			return fail("link duplicate, identity or clock")
		}
		if v.Key.Type != "parent" && v.Key.Type != "blocks" && v.Key.Type != "related" {
			return fail("link kind")
		}
		if v.Key.Type == "related" && v.Key.FromUID > v.Key.ToUID {
			return fail("noncanonical related link")
		}
		_, from := state.Fold.Issues[v.Key.FromUID]
		_, to := state.Fold.Issues[v.Key.ToUID]
		if !from && !to {
			return fail("link has no captured endpoint")
		}
		// A missing peer is a valid deferred edge. Do not guess its project.
		state.Fold.Links[v.Key] = v.State
	}
	for _, v := range f.IssueMetadata {
		if _, ok := state.Fold.IssueMetadata[v.UID]; ok {
			return fail("duplicate issue metadata")
		}
		if _, ok := state.Fold.Issues[v.UID]; !ok {
			return fail("issue metadata owner")
		}
		if err := validateCaptureMetadata(v.Value, true); err != nil {
			return fail(err.Error())
		}
		state.Fold.IssueMetadata[v.UID] = v.Value
	}
	for _, v := range f.ProjectMetadata {
		if _, ok := state.Fold.ProjectMetadata[v.UID]; ok || !projects[v.UID] {
			return fail("project metadata owner or duplicate")
		}
		if err := validateCaptureMetadata(v.Value, false); err != nil {
			return fail(err.Error())
		}
		state.Fold.ProjectMetadata[v.UID] = v.Value
	}
	for _, v := range f.CronJobs {
		if _, ok := state.Fold.CronJobs[v.UID]; ok || !projects[v.ProjectUID] || v.ID != 0 || v.ProjectID != 0 {
			return fail("job ownership, duplicate or local row ID")
		}
		if err := validateCaptureDefinition(v.CronDefinition); err != nil {
			return fail(err.Error())
		}
		raw, err := json.Marshal(v.Definition)
		if err != nil {
			return fail(err.Error())
		}
		if _, err = cron.DecodeJob(raw); err != nil {
			return fail(err.Error())
		}
		if v.Definition.Version != 1 || v.Definition.Kind != "job" {
			return fail("job document version or kind")
		}
		if !validCaptureJobReferences(v.Definition) {
			return fail("job reference identity")
		}
		state.Fold.CronJobs[v.UID] = v
	}
	for _, v := range f.CronWorkflows {
		if _, ok := state.Fold.CronWorkflows[v.UID]; ok || !projects[v.ProjectUID] || v.ID != 0 || v.ProjectID != 0 {
			return fail("workflow ownership, duplicate or local row ID")
		}
		if err := validateCaptureDefinition(v.CronDefinition); err != nil {
			return fail(err.Error())
		}
		raw, err := json.Marshal(v.Definition)
		if err != nil {
			return fail(err.Error())
		}
		if _, err = cron.DecodeWorkflow(raw); err != nil {
			return fail(err.Error())
		}
		if v.Definition.Version != 1 {
			return fail("workflow document version")
		}
		state.Fold.CronWorkflows[v.UID] = v
	}
	for _, v := range f.CronRuns {
		if _, ok := state.Fold.CronRuns[v.UID]; ok || !projects[v.ProjectUID] || v.ID != 0 || v.ProjectID != 0 || !uid.Valid(v.ObservationEventUID) {
			return fail("run ownership, duplicate, row ID or provenance")
		}
		if err := v.ObservationHLC.Validate(); err != nil {
			return fail(err.Error())
		}
		portable := CronRunExport(v.CronRun)
		portable.ID = 1
		portable.ProjectID = 1
		if err := ValidateCronRecord(&portable); err != nil {
			return fail(err.Error())
		}
		state.Fold.CronRuns[v.UID] = v
	}
	seen := map[string]bool{}
	for _, v := range w.Creation {
		key := v.ProjectUID + ":" + v.Kind + ":" + v.EntityUID
		if seen[key] || !projects[v.ProjectUID] || !uid.Valid(v.EntityUID) || v.AttributionStatus == "" {
			return fail("creation duplicate, identity or attribution")
		}
		seen[key] = true
		owned := false
		switch v.Kind {
		case "project":
			owned = v.EntityUID == v.ProjectUID
		case "issue":
			owned = ownsIssue(v.EntityUID, v.ProjectUID)
		case "comment":
			c, ok := state.Fold.Comments[v.EntityUID]
			owned = ok && ownsIssue(c.IssueUID, v.ProjectUID)
		case "cron_job":
			d, ok := state.Fold.CronJobs[v.EntityUID]
			owned = ok && d.ProjectUID == v.ProjectUID
		case "cron_workflow":
			d, ok := state.Fold.CronWorkflows[v.EntityUID]
			owned = ok && d.ProjectUID == v.ProjectUID
		}
		if !owned {
			return fail("creation kind or entity ownership")
		}
	}
	seen = map[string]bool{}
	for _, v := range w.Close {
		issue, ok := state.Fold.Issues[v.IssueUID]
		if !ok || seen[v.IssueUID] || v.StatusIntentUID != issue.StatusIntentUID || v.StatusClock != issue.StatusClock || !sameCloseBoundary(v.CloseBoundaryClock, issue.CloseBoundaryClock) || !sameCloseValue(v.Value, issue.CloseValue) {
			return fail("close ownership, duplicate or status proof")
		}
		seen[v.IssueUID] = true
		if err := ValidateScopedCanonicalCloseState(v, v.IssueUID, issue.Status); err != nil {
			return fail(err.Error())
		}
		if v.Value != nil {
			if issue.Status != "closed" || v.StatusIntentUID == "" {
				return fail("close value without effective closed intent")
			}
			if err := v.Value.Validate(); err != nil {
				return fail(err.Error())
			}
			if peer, ok := state.Fold.Issues[v.Value.PeerIssueUID]; ok && peer.ProjectUID != v.Value.PeerProjectUID {
				return fail("close peer project mismatch")
			}
		}
	}
	seen = map[string]bool{}
	for _, v := range w.Artifacts {
		key := v.ProjectUID + ":" + v.Digest
		if seen[key] || !ownsIssue(v.IssueUID, v.ProjectUID) {
			return fail("artifact owner or duplicate")
		}
		seen[key] = true
		if err := embedding.ValidateArtifactManifest(v); err != nil {
			return fail(err.Error())
		}
	}
	return state, nil
}

func validCaptureClock(c FoldClock) bool {
	return c == (FoldClock{}) || (c.HLCPhysicalMS >= 0 && c.HLCCounter >= 0 && uid.Valid(c.OriginInstanceUID) && uid.Valid(c.EventUID))
}
func validateCaptureDefinition(d CronDefinition) error {
	// Native backup validation requires backend IDs. Supply validation-only
	// stand-ins on this value copy; the portable document keeps zero IDs.
	d.ID = 1
	d.ProjectID = 1
	return validateCronDefinition(d)
}
func validCaptureJobReferences(d cron.JobDefinition) bool {
	refs := []string{d.Trigger.IssueUID, d.Action.WorkflowUID}
	if d.Issue != nil {
		refs = append(refs, d.Issue.UID, d.Issue.ParentUID)
	}
	for _, v := range refs {
		if v != "" && !uid.Valid(v) {
			return false
		}
	}
	return true
}

func validateCaptureMetadata(raw []byte, issue bool) error {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("capture metadata must be an object")
	}
	registry := metadata.ProjectRegistry
	if issue {
		registry = metadata.IssueRegistry
	}
	for key, value := range object {
		if err := metadata.Validate(registry, key, value); err != nil {
			return err
		}
		// Native recurring metadata is machine-owned. Validate its known typed
		// version without projecting, stripping or reinterpreting authored values.
		if issue && key == "cron.occurrence" {
			var occurrence struct {
				Version            int    `json:"version"`
				JobUID             string `json:"job_uid"`
				DefinitionEventUID string `json:"definition_event_uid"`
				ScheduledAt        string `json:"scheduled_at"`
			}
			if err := json.Unmarshal(value, &occurrence, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if occurrence.Version != 1 || !uid.Valid(occurrence.JobUID) || !uid.Valid(occurrence.DefinitionEventUID) || occurrence.ScheduledAt == "" {
				return errors.New("invalid capture recurring occurrence")
			}
		}
		if issue && strings.HasPrefix(key, "cron.progress.") {
			if !uid.Valid(strings.TrimPrefix(key, "cron.progress.")) {
				return errors.New("invalid capture recurring progress identity")
			}
			var progress cron.RecurringProgress
			if err := json.Unmarshal(value, &progress, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if err := progress.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ScopedCaptureTime preserves the native current-row timestamp encoding.
func ScopedCaptureTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	result := value.UTC().Format(EventTimestampFormat)
	return &result
}

// ScopedCaptureCreation copies native attribution into unsigned capture data;
// it grants no authority and creates no receipt or signature.
func ScopedCaptureCreation(project, kind, entity, author, teammate string, attribution AttributionView) ScopedProvenance {
	status := attribution.Verification
	if status == "" {
		status = "legacy"
	}
	if attribution.SourceActor != "" {
		author = attribution.SourceActor
	}
	if attribution.Teammate != "" {
		teammate = attribution.Teammate
	}
	return ScopedProvenance{ProjectUID: project, Kind: kind, EntityUID: entity, Creator: author, Teammate: teammate, AttributionStatus: status, Signatures: []ScopedSignature{}}
}
