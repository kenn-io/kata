package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/embedding"
)

// scopedDefinitionIdentity preserves the native job/workflow UID namespaces.
type scopedDefinitionIdentity struct {
	projectUID string
	kind       string
	uid        string
}

// ProjectScopedState builds a complete current view from immutable canonical
// facts. Capture clocks, source envelopes and historical close prose have no
// path into these explicitly constructed records. This function never folds,
// reads storage, allocates a sequence, or signs a record.
func ProjectScopedState(c CanonicalViewState, view ScopedViewDescriptor) (ScopedCollections, error) {
	if err := view.Validate(); err != nil {
		return ScopedCollections{}, err
	}
	out := emptyScopedCollections()
	projects := map[string]bool{}
	for _, pin := range view.Projects {
		projects[pin.ProjectUID] = true
	}
	for _, p := range c.Projects {
		if !projects[p.UID] || p.DeletedAt != nil {
			continue
		}
		meta := p.Metadata
		if raw, ok := c.Fold.ProjectMetadata[p.UID]; ok {
			meta = JSONBlob(raw)
		}
		out.Projects = append(out.Projects, ScopedProjectState{UID: p.UID, Name: p.Name, Metadata: meta})
	}
	availableProjects := map[string]bool{}
	for _, p := range out.Projects {
		availableProjects[p.UID] = true
	}
	issues := map[string]ScopedIssueState{}
	endpoints := map[string]ScopedEndpoint{}
	for _, i := range c.Fold.SortedIssues() {
		if !availableProjects[i.ProjectUID] {
			continue
		}
		created, err := scopedTime(i.CreatedAt)
		if err != nil {
			return ScopedCollections{}, err
		}
		var expires *time.Time
		if i.AssignmentExpiresOn != nil {
			v, err := scopedTime(*i.AssignmentExpiresOn)
			if err != nil {
				return ScopedCollections{}, err
			}
			expires = &v
		}
		v := ScopedIssueState{UID: i.UID, ProjectUID: i.ProjectUID, ShortID: i.ShortID, Title: i.Title, Body: i.Body, Author: i.Author, Status: i.Status, Owner: i.Owner, Priority: i.Priority, AssignmentExpiresOn: expires, CreatedAt: created, Metadata: JSONBlob(c.Fold.IssueMetadata[i.UID]), Deleted: i.DeletedAt != nil}
		out.Issues = append(out.Issues, v)
		issues[i.UID] = v
		if !v.Deleted {
			endpoints[i.UID] = ScopedEndpoint{ProjectUID: i.ProjectUID, IssueUID: i.UID}
		}
	}
	closes := map[string]CanonicalCloseState{}
	for _, v := range c.Close {
		if _, exists := closes[v.IssueUID]; exists {
			return ScopedCollections{}, errors.New("duplicate canonical close proof")
		}
		closes[v.IssueUID] = v
	}
	parents := map[string]ScopedEndpoint{}
	for key, state := range c.Fold.Links {
		if !state.Present {
			continue
		}
		switch key.Type {
		case "parent", "blocks", "related":
		default:
			return ScopedCollections{}, errors.New("unknown canonical structured link family")
		}
		subject, ok := endpoints[key.FromUID]
		if !ok {
			continue
		}
		peer, ok := endpoints[key.ToUID]
		if !ok {
			continue
		}
		if key.FromUID == key.ToUID {
			return ScopedCollections{}, errors.New("canonical self link")
		}
		if key.Type == "parent" {
			if _, exists := parents[key.FromUID]; exists {
				return ScopedCollections{}, errors.New("multiple canonical parents")
			}
			parents[key.FromUID] = peer
			continue
		}
		if key.Type == "related" && subject.IssueUID > peer.IssueUID {
			subject, peer = peer, subject
		}
		out.References = append(out.References, ScopedReference{Kind: "link", Slot: "link:" + key.Type + ":" + subject.IssueUID + ":" + peer.IssueUID, Subject: subject, Peer: &peer, Relation: key.Type, Present: true, Revision: 1})
	}
	for _, i := range out.Issues {
		subject := ScopedEndpoint{ProjectUID: i.ProjectUID, IssueUID: i.UID}
		parent := ScopedReference{Kind: "parent", Slot: "parent", Subject: subject, Revision: 1}
		if peer, ok := parents[i.UID]; ok {
			parent.Peer = &peer
			parent.Present = true
		}
		closeRef := ScopedReference{Kind: "close", Slot: "close", Subject: subject, Revision: 1}
		canonical := c.Fold.Issues[i.UID]
		proof, ok := closes[i.UID]
		if !i.Deleted && i.Status == "closed" && ok && canonical.StatusIntentUID != "" && proof.StatusIntentUID == canonical.StatusIntentUID && proof.StatusClock == canonical.StatusClock && proof.Value != nil {
			if err := proof.Value.Validate(); err != nil {
				return ScopedCollections{}, err
			}
			if peer, visible := endpoints[proof.Value.PeerIssueUID]; visible && peer.ProjectUID == proof.Value.PeerProjectUID {
				value := *proof.Value
				closeRef.Close = &value
				closeRef.Present = true
			}
		}
		out.References = append(out.References, parent, closeRef)
	}
	comments := map[string]ScopedCommentState{}
	for _, v := range c.Fold.SortedComments() {
		owner, ok := issues[v.IssueUID]
		if !ok {
			continue
		}
		created, err := scopedTime(v.CreatedAt)
		if err != nil {
			return ScopedCollections{}, err
		}
		comment := ScopedCommentState{UID: v.UID, ProjectUID: owner.ProjectUID, IssueUID: v.IssueUID, Author: v.Author, Teammate: v.Teammate, Body: v.Body, CreatedAt: created}
		out.Comments = append(out.Comments, comment)
		comments[v.UID] = comment
	}
	for _, v := range out.Comments {
		canonical := c.Fold.Comments[v.UID]
		r := ScopedReference{Kind: "reply", Slot: "reply", Subject: ScopedEndpoint{ProjectUID: v.ProjectUID, IssueUID: v.IssueUID}, SubjectEntityUID: v.UID, Revision: 1}
		if peer, ok := comments[canonical.ReplyToUID]; ok && !issues[v.IssueUID].Deleted && !issues[peer.IssueUID].Deleted {
			if err := ValidateCommentReply(v.UID, peer.UID, canonical.ReplyKind); err != nil {
				return ScopedCollections{}, err
			}
			r.Peer = &ScopedEndpoint{ProjectUID: peer.ProjectUID, IssueUID: peer.IssueUID}
			r.PeerEntityUID = peer.UID
			r.Relation = canonical.ReplyKind
			r.Present = true
		}
		out.References = append(out.References, r)
	}
	for _, v := range c.Fold.PresentLabels() {
		if e, ok := endpoints[v.IssueUID]; ok {
			out.Labels = append(out.Labels, ScopedLabelState{ProjectUID: e.ProjectUID, IssueUID: e.IssueUID, Label: v.Label})
		}
	}
	// Definitions have typed reference slots. A required unavailable target
	// withdraws the whole definition instead of silently changing its meaning.
	workflows := map[scopedDefinitionIdentity]ScopedDefinitionState{}
	for _, v := range c.Fold.CronWorkflows {
		if !availableProjects[v.ProjectUID] {
			continue
		}
		def := v.Definition
		value := ScopedDefinitionState{UID: v.UID, ProjectUID: v.ProjectUID, Kind: "workflow", Name: v.Name, Author: v.Author, CreatedAt: v.CreatedAt, Deleted: v.DeletedAt != nil, Workflow: &def}
		if err := value.Validate(); err != nil {
			return ScopedCollections{}, err
		}
		out.Definitions = append(out.Definitions, value)
		workflows[scopedDefinitionIdentity{value.ProjectUID, "workflow", value.UID}] = value
	}
	for _, v := range c.Fold.CronJobs {
		if !availableProjects[v.ProjectUID] {
			continue
		}
		def := v.Definition
		value := ScopedDefinitionState{UID: v.UID, ProjectUID: v.ProjectUID, Kind: "job", Name: v.Name, Author: v.Author, CreatedAt: v.CreatedAt, Deleted: v.DeletedAt != nil, Job: &def}
		if err := value.Validate(); err != nil {
			return ScopedCollections{}, err
		}
		refs := []ScopedReference{}
		safe := true
		add := func(slot, id string, workflow bool) {
			if id == "" {
				return
			}
			r := ScopedReference{Kind: "definition", Slot: slot, Subject: ScopedEndpoint{ProjectUID: v.ProjectUID}, SubjectEntityUID: v.UID, Revision: 1, Present: true}
			if workflow {
				canonical, known := c.Fold.CronWorkflows[id]
				target, ok := workflows[scopedDefinitionIdentity{canonical.ProjectUID, "workflow", id}]
				if !known || !ok || target.Deleted {
					safe = false
					return
				}
				r.Peer = &ScopedEndpoint{ProjectUID: target.ProjectUID}
				r.PeerEntityUID = id
				r.Relation = "workflow"
			} else {
				peer, ok := endpoints[id]
				if !ok {
					safe = false
					return
				}
				r.Peer = &peer
				r.Relation = "issue"
			}
			refs = append(refs, r)
		}
		add("trigger.issue", def.Trigger.IssueUID, false)
		add("action.workflow", def.Action.WorkflowUID, true)
		def.Trigger.IssueUID = ""
		def.Action.WorkflowUID = ""
		if def.Issue != nil {
			policy := *def.Issue
			add("issue.uid", policy.UID, false)
			add("issue.parent", policy.ParentUID, false)
			policy.UID = ""
			policy.ParentUID = ""
			def.Issue = &policy
		}
		if safe {
			out.Definitions = append(out.Definitions, value)
			out.References = append(out.References, refs...)
		}
	}
	projectedDefinitions := map[scopedDefinitionIdentity]bool{}
	for _, definition := range out.Definitions {
		projectedDefinitions[scopedDefinitionIdentity{definition.ProjectUID, definition.Kind, definition.UID}] = true
	}
	for _, v := range c.Creation {
		if !availableProjects[v.ProjectUID] {
			continue
		}
		visible := false
		switch v.Kind {
		case "project":
			visible = v.EntityUID == v.ProjectUID
		case "issue":
			i, ok := issues[v.EntityUID]
			visible = ok && i.ProjectUID == v.ProjectUID
		case "comment":
			i, ok := comments[v.EntityUID]
			visible = ok && i.ProjectUID == v.ProjectUID
		case "cron_job", "cron_workflow":
			visible = projectedDefinitions[scopedDefinitionIdentity{v.ProjectUID, strings.TrimPrefix(v.Kind, "cron_"), v.EntityUID}]
		default:
			return ScopedCollections{}, errors.New("unknown canonical creation family")
		}
		if visible {
			v.Signatures = nil
			out.Provenance = append(out.Provenance, v)
		}
	}
	artifacts, err := scopedArtifacts(c.Artifacts, issues)
	if err != nil {
		return ScopedCollections{}, err
	}
	out.Artifacts = artifacts
	return normalizeScopedCollections(out)
}

func scopedTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	v, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid canonical visible time: %w", err)
	}
	return v.UTC(), nil
}
func emptyScopedCollections() ScopedCollections {
	return ScopedCollections{Projects: []ScopedProjectState{}, Issues: []ScopedIssueState{}, Comments: []ScopedCommentState{}, Labels: []ScopedLabelState{}, Definitions: []ScopedDefinitionState{}, References: []ScopedReference{}, Provenance: []ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
}

// RestrictScopedState is the typed current-content adapter for a narrower
// caller. It does not return transport endorsements or view-wide positions.
// Nil filters admit a dimension; explicit empty filters deny it. Any nonnil
// issue filter excludes project-wide definitions and their endorsements.
// The authorized project catalog remains independent of issue selection.
func RestrictScopedState(c ScopedCollections, projectUIDs, issueUIDs []string) (ScopedCollections, error) {
	if err := c.Validate(); err != nil {
		return ScopedCollections{}, err
	}
	out := emptyScopedCollections()
	projects := map[string]bool{}
	issues := map[string]ScopedIssueState{}
	comments := map[string]bool{}
	definitions := map[scopedDefinitionIdentity]ScopedDefinitionState{}
	for _, p := range c.Projects {
		if projectUIDs == nil || slices.Contains(projectUIDs, p.UID) {
			out.Projects = append(out.Projects, p)
			projects[p.UID] = true
		}
	}
	for _, i := range c.Issues {
		if projects[i.ProjectUID] && (issueUIDs == nil || slices.Contains(issueUIDs, i.UID)) {
			out.Issues = append(out.Issues, i)
			issues[i.UID] = i
		}
	}
	available := func(e ScopedEndpoint) bool {
		i, ok := issues[e.IssueUID]
		return ok && !i.Deleted && i.ProjectUID == e.ProjectUID
	}
	for _, v := range c.Comments {
		owner, retained := issues[v.IssueUID]
		if retained && owner.ProjectUID == v.ProjectUID {
			out.Comments = append(out.Comments, v)
			comments[v.UID] = true
		}
	}
	for _, v := range c.Labels {
		if available(ScopedEndpoint{ProjectUID: v.ProjectUID, IssueUID: v.IssueUID}) {
			out.Labels = append(out.Labels, v)
		}
	}
	for _, v := range c.Definitions {
		if issueUIDs == nil && projects[v.ProjectUID] {
			definitions[scopedDefinitionIdentity{v.ProjectUID, v.Kind, v.UID}] = v
		}
	}
	workflowAvailable := func(peer *ScopedEndpoint, entityUID string) bool {
		if peer == nil || !projects[peer.ProjectUID] {
			return false
		}
		target, ok := definitions[scopedDefinitionIdentity{peer.ProjectUID, "workflow", entityUID}]
		return ok && !target.Deleted
	}
	// Removing a mandatory workflow can withdraw dependent jobs. Iterate until
	// stable so the result does not depend on definition/reference input order.
	changed := true
	for changed {
		changed = false
		for _, r := range c.References {
			if r.Kind != "definition" || !r.Present {
				continue
			}
			subject := scopedDefinitionIdentity{r.Subject.ProjectUID, "job", r.SubjectEntityUID}
			if _, selected := definitions[subject]; !selected {
				continue
			}
			safe := false
			if r.Relation == "workflow" {
				safe = workflowAvailable(r.Peer, r.PeerEntityUID)
			} else {
				safe = r.Peer != nil && available(*r.Peer)
			}
			if !safe {
				delete(definitions, subject)
				changed = true
			}
		}
	}
	for _, v := range c.Definitions {
		if _, selected := definitions[scopedDefinitionIdentity{v.ProjectUID, v.Kind, v.UID}]; selected {
			out.Definitions = append(out.Definitions, v)
		}
	}
	for _, r := range c.References {
		subject, ok := issues[r.Subject.IssueUID]
		switch r.Kind {
		case "definition":
			if _, selected := definitions[scopedDefinitionIdentity{r.Subject.ProjectUID, "job", r.SubjectEntityUID}]; !selected {
				continue
			}
		case "reply":
			if !ok || subject.ProjectUID != r.Subject.ProjectUID || !comments[r.SubjectEntityUID] {
				continue
			}
		default:
			if !ok || subject.ProjectUID != r.Subject.ProjectUID {
				continue
			}
		}
		visible := r.Peer == nil || available(*r.Peer)
		if r.Kind == "definition" && r.Relation == "workflow" {
			visible = workflowAvailable(r.Peer, r.PeerEntityUID)
		}
		if r.Kind == "reply" {
			visible = visible && !subject.Deleted && (!r.Present || comments[r.PeerEntityUID])
		}
		if r.Close != nil {
			visible = available(ScopedEndpoint{ProjectUID: r.Close.PeerProjectUID, IssueUID: r.Close.PeerIssueUID})
		}
		if !visible {
			if r.Kind == "link" || r.Kind == "definition" {
				continue
			}
			r.Peer = nil
			r.Close = nil
			r.PeerEntityUID = ""
			r.Relation = ""
			r.Present = false
		}
		r.Revision = 1
		r.Signatures = nil
		out.References = append(out.References, r)
	}
	for _, v := range c.Provenance {
		visible := false
		switch v.Kind {
		case "project":
			visible = projects[v.ProjectUID] && v.EntityUID == v.ProjectUID
		case "issue":
			i, ok := issues[v.EntityUID]
			visible = ok && i.ProjectUID == v.ProjectUID
		case "comment":
			visible = comments[v.EntityUID]
		case "cron_job", "cron_workflow":
			_, visible = definitions[scopedDefinitionIdentity{v.ProjectUID, strings.TrimPrefix(v.Kind, "cron_"), v.EntityUID}]
		default:
			return ScopedCollections{}, errors.New("unknown scoped provenance family")
		}
		if visible {
			v.Signatures = nil
			out.Provenance = append(out.Provenance, v)
		}
	}
	artifacts, err := scopedArtifacts(c.Artifacts, issues)
	if err != nil {
		return ScopedCollections{}, err
	}
	out.Artifacts = artifacts
	return normalizeScopedCollections(out)
}

// ScopedStateDigest is a content ETag, never a mutation precondition. It
// excludes transport signatures and observation positions, not visible facts.
func ScopedStateDigest(c ScopedCollections) (string, error) {
	c, err := normalizeScopedCollections(c)
	if err != nil {
		return "", err
	}
	for n := range c.References {
		c.References[n].Revision = 1
		c.References[n].Signatures = nil
	}
	for n := range c.Provenance {
		c.Provenance[n].Signatures = nil
	}
	raw, err := scopedCanonicalJSON(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// DiffScopedState compares complete current collections before requiring a
// sequence. Hidden-only/no-effect captures return no effects even at position 0.
// Every visible replacement/removal in a batch uses one scope-wide position.
func DiffScopedState(before, after ScopedCollections, position int64) ([]ScopedEffect, error) {
	before, err := normalizeScopedCollections(before)
	if err != nil {
		return nil, err
	}
	after, err = normalizeScopedCollections(after)
	if err != nil {
		return nil, err
	}
	old, err := scopedCollectionRows(before)
	if err != nil {
		return nil, err
	}
	next, err := scopedCollectionRows(after)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(old)+len(next))
	for k := range old {
		keys = append(keys, k)
	}
	for k := range next {
		if _, ok := old[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	effects := []ScopedEffect{}
	for _, key := range keys {
		a, aok := old[key]
		b, bok := next[key]
		if aok && bok {
			aa, err := scopedEffectContent(a)
			if err != nil {
				return nil, err
			}
			bb, err := scopedEffectContent(b)
			if err != nil {
				return nil, err
			}
			if string(aa) == string(bb) {
				continue
			}
		}
		if position <= 0 {
			return nil, errors.New("visible effects require positive scope position")
		}
		if !bok {
			b = ScopedEffect{Collection: a.Collection, Key: a.Key, Action: "remove"}
		} else {
			b.Action = "replace"
			if b.Reference != nil {
				ref := *b.Reference
				ref.Revision = position
				ref.Signatures = nil
				b.Reference = &ref
			}
		}
		b.Revision = position
		effects = append(effects, b)
	}
	return effects, nil
}
func scopedEffectContent(e ScopedEffect) ([]byte, error) {
	e.Revision = 0
	e.Action = ""
	if e.Reference != nil {
		v := *e.Reference
		v.Revision = 1
		v.Signatures = nil
		e.Reference = &v
	}
	if e.Provenance != nil {
		v := *e.Provenance
		v.Signatures = nil
		e.Provenance = &v
	}
	return scopedCanonicalJSON(e)
}
func scopedCanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return canonical, nil
}

// Keys are collection-local stable identities. Slot keys never incorporate the
// current value of a nullable register. Typed loops keep the inventory closed.
func scopedCollectionRows(c ScopedCollections) (map[string]ScopedEffect, error) {
	rows := map[string]ScopedEffect{}
	add := func(e ScopedEffect) { rows[e.Collection+"/"+e.Key] = e }
	for _, v := range c.Projects {
		add(ScopedEffect{Collection: "projects", Key: v.UID, Project: &v})
	}
	for _, v := range c.Issues {
		add(ScopedEffect{Collection: "issues", Key: v.ProjectUID + ":" + v.UID, Issue: &v})
	}
	for _, v := range c.Comments {
		add(ScopedEffect{Collection: "comments", Key: v.ProjectUID + ":" + v.UID, Comment: &v})
	}
	for _, v := range c.Labels {
		add(ScopedEffect{Collection: "labels", Key: v.ProjectUID + ":" + v.IssueUID + ":" + v.Label, Label: &v})
	}
	for _, v := range c.Definitions {
		key, err := ScopedDefinitionKey(v.ProjectUID, v.Kind, v.UID)
		if err != nil {
			return nil, err
		}
		add(ScopedEffect{Collection: "definitions", Key: key, Definition: &v})
	}
	for _, v := range c.References {
		add(ScopedEffect{Collection: "references", Key: v.Subject.ProjectUID + ":" + v.Subject.IssueUID + ":" + v.SubjectEntityUID + ":" + v.Slot, Reference: &v})
	}
	for _, v := range c.Provenance {
		add(ScopedEffect{Collection: "provenance", Key: v.ProjectUID + ":" + v.Kind + ":" + v.EntityUID, Provenance: &v})
	}
	for _, v := range c.Artifacts {
		add(ScopedEffect{Collection: "artifacts", Key: v.ProjectUID + ":" + v.Digest, Artifact: &v})
	}
	return rows, nil
}
func normalizeScopedCollections(c ScopedCollections) (ScopedCollections, error) {
	if err := c.Validate(); err != nil {
		return ScopedCollections{}, err
	}
	// Deep-copy JSON documents and pointer fields. Callers may mutate returned
	// current state without altering retained immutable capture or input records.
	raw, err := json.Marshal(c)
	if err != nil {
		return ScopedCollections{}, err
	}
	var copy ScopedCollections
	if err = json.Unmarshal(raw, &copy); err != nil {
		return ScopedCollections{}, err
	}
	c = copy
	for n := range c.Projects {
		v, err := scopedMetadata(c.Projects[n].Metadata)
		if err != nil {
			return ScopedCollections{}, err
		}
		c.Projects[n].Metadata = v
	}
	for n := range c.Issues {
		v, err := scopedMetadata(c.Issues[n].Metadata)
		if err != nil {
			return ScopedCollections{}, err
		}
		c.Issues[n].Metadata = v
	}
	if err = c.Validate(); err != nil {
		return ScopedCollections{}, err
	}
	rows, err := scopedCollectionRows(c)
	if err != nil {
		return ScopedCollections{}, err
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := emptyScopedCollections()
	for _, key := range keys {
		e := rows[key]
		switch e.Collection {
		case "projects":
			out.Projects = append(out.Projects, *e.Project)
		case "issues":
			out.Issues = append(out.Issues, *e.Issue)
		case "comments":
			out.Comments = append(out.Comments, *e.Comment)
		case "labels":
			out.Labels = append(out.Labels, *e.Label)
		case "definitions":
			out.Definitions = append(out.Definitions, *e.Definition)
		case "references":
			out.References = append(out.References, *e.Reference)
		case "provenance":
			out.Provenance = append(out.Provenance, *e.Provenance)
		case "artifacts":
			out.Artifacts = append(out.Artifacts, *e.Artifact)
		}
	}
	return out, nil
}
func scopedMetadata(v JSONBlob) (JSONBlob, error) {
	if v == "" {
		return JSONBlob(`{}`), nil
	}
	raw := jsontext.Value(v)
	if err := raw.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(raw), "{") {
		return "", errors.New("scoped metadata must be an object")
	}
	return JSONBlob(raw), nil
}
