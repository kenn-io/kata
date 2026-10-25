//go:build rcpl_private

package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// ScopedOracle is a fixture oracle, not a store/capture implementation. It
// independently constructs typed current state and applies project/issue
// visibility. It must never call the production projector, its reference
// predicate, normalization, digest, or restriction helpers. Nil filters admit
// a dimension; explicit empty filters deny it. An issue-restricted principal
// cannot read project-level definitions, even with a project catalog entry.
func ScopedOracle(c db.CanonicalViewState, projectIDs, issueIDs []string) (db.ScopedCollections, error) {
	out := db.ScopedCollections{Projects: []db.ScopedProjectState{}, Issues: []db.ScopedIssueState{}, Comments: []db.ScopedCommentState{}, Labels: []db.ScopedLabelState{}, Definitions: []db.ScopedDefinitionState{}, References: []db.ScopedReference{}, Provenance: []db.ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
	includes := func(ids []string, id string) bool {
		if ids == nil {
			return true
		}
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}
	metadata := func(raw db.JSONBlob) (db.JSONBlob, error) {
		if raw == "" {
			return db.JSONBlob(`{}`), nil
		}
		v := jsontext.Value(raw)
		if err := v.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
			return "", err
		}
		return db.JSONBlob(v), nil
	}
	date := func(raw string) (time.Time, error) {
		if raw == "" {
			return time.Time{}, nil
		}
		v, err := time.Parse(time.RFC3339Nano, raw)
		return v.UTC(), err
	}
	projects := map[string]bool{}
	issues := map[string]db.ScopedIssueState{}
	comments := map[string]db.ScopedCommentState{}
	type oracleDefinitionIdentity struct {
		project string
		kind    string
		entity  string
	}
	definitions := map[oracleDefinitionIdentity]db.ScopedDefinitionState{}
	for _, p := range c.Projects {
		if !includes(projectIDs, p.UID) || p.DeletedAt != nil {
			continue
		}
		raw := p.Metadata
		if v, ok := c.Fold.ProjectMetadata[p.UID]; ok {
			raw = db.JSONBlob(v)
		}
		m, err := metadata(raw)
		if err != nil {
			return out, err
		}
		projects[p.UID] = true
		out.Projects = append(out.Projects, db.ScopedProjectState{UID: p.UID, Name: p.Name, Metadata: m})
	}
	for _, i := range c.Fold.Issues {
		if !projects[i.ProjectUID] || !includes(issueIDs, i.UID) {
			continue
		}
		created, err := date(i.CreatedAt)
		if err != nil {
			return out, err
		}
		m, err := metadata(db.JSONBlob(c.Fold.IssueMetadata[i.UID]))
		if err != nil {
			return out, err
		}
		v := db.ScopedIssueState{UID: i.UID, ProjectUID: i.ProjectUID, ShortID: i.ShortID, Title: i.Title, Body: i.Body, Author: i.Author, Status: i.Status, Owner: i.Owner, Priority: i.Priority, CreatedAt: created, Metadata: m, Deleted: i.DeletedAt != nil}
		if i.AssignmentExpiresOn != nil {
			when, err := date(*i.AssignmentExpiresOn)
			if err != nil {
				return out, err
			}
			v.AssignmentExpiresOn = &when
		}
		issues[i.UID] = v
		out.Issues = append(out.Issues, v)
	}
	for _, v := range c.Fold.Comments {
		i, ok := issues[v.IssueUID]
		if !ok {
			continue
		}
		created, err := date(v.CreatedAt)
		if err != nil {
			return out, err
		}
		comments[v.UID] = db.ScopedCommentState{UID: v.UID, ProjectUID: i.ProjectUID, IssueUID: i.UID, Author: v.Author, Teammate: v.Teammate, Body: v.Body, CreatedAt: created}
		out.Comments = append(out.Comments, comments[v.UID])
	}
	for key, v := range c.Fold.Labels {
		i, ok := issues[key.IssueUID]
		if ok && !i.Deleted && v.Present {
			out.Labels = append(out.Labels, db.ScopedLabelState{ProjectUID: i.ProjectUID, IssueUID: i.UID, Label: key.Label})
		}
	}
	for id, i := range issues {
		parent := db.ScopedReference{Kind: "parent", Slot: "parent", Subject: db.ScopedEndpoint{ProjectUID: i.ProjectUID, IssueUID: id}, Revision: 1}
		closeRef := db.ScopedReference{Kind: "close", Slot: "close", Subject: parent.Subject, Revision: 1}
		if !i.Deleted {
			for key, v := range c.Fold.Links {
				peer, ok := issues[key.ToUID]
				if v.Present && key.Type == "parent" && key.FromUID == id && ok && !peer.Deleted {
					parent.Present = true
					parent.Peer = &db.ScopedEndpoint{ProjectUID: peer.ProjectUID, IssueUID: peer.UID}
				}
			}
			canonical := c.Fold.Issues[id]
			for _, proof := range c.Close {
				if proof.IssueUID != id || i.Status != "closed" || canonical.StatusIntentUID == "" || proof.StatusIntentUID != canonical.StatusIntentUID || proof.StatusClock != canonical.StatusClock || proof.Value == nil {
					continue
				}
				peer, ok := issues[proof.Value.PeerIssueUID]
				if ok && !peer.Deleted && peer.ProjectUID == proof.Value.PeerProjectUID {
					value := *proof.Value
					closeRef.Close = &value
					closeRef.Present = true
				}
			}
		}
		out.References = append(out.References, parent, closeRef)
	}
	for key, state := range c.Fold.Links {
		if !state.Present || key.Type == "parent" {
			continue
		}
		if key.Type != "blocks" && key.Type != "related" {
			return out, fmt.Errorf("oracle unsupported relation %q", key.Type)
		}
		a, aok := issues[key.FromUID]
		b, bok := issues[key.ToUID]
		if !aok || !bok || a.Deleted || b.Deleted {
			continue
		}
		if key.Type == "related" && a.UID > b.UID {
			a, b = b, a
		}
		out.References = append(out.References, db.ScopedReference{Kind: "link", Slot: "link:" + key.Type + ":" + a.UID + ":" + b.UID, Subject: db.ScopedEndpoint{ProjectUID: a.ProjectUID, IssueUID: a.UID}, Peer: &db.ScopedEndpoint{ProjectUID: b.ProjectUID, IssueUID: b.UID}, Relation: key.Type, Present: true, Revision: 1})
	}
	for id, v := range comments {
		r := db.ScopedReference{Kind: "reply", Slot: "reply", Subject: db.ScopedEndpoint{ProjectUID: v.ProjectUID, IssueUID: v.IssueUID}, SubjectEntityUID: id, Revision: 1}
		original := c.Fold.Comments[id]
		if peer, ok := comments[original.ReplyToUID]; ok && !issues[v.IssueUID].Deleted && !issues[peer.IssueUID].Deleted {
			r.Peer = &db.ScopedEndpoint{ProjectUID: peer.ProjectUID, IssueUID: peer.IssueUID}
			r.PeerEntityUID = peer.UID
			r.Relation = original.ReplyKind
			r.Present = true
		}
		out.References = append(out.References, r)
	}
	if issueIDs == nil {
		for _, w := range c.Fold.CronWorkflows {
			if !projects[w.ProjectUID] {
				continue
			}
			doc := w.Definition
			if doc.Version != 1 {
				return out, fmt.Errorf("oracle unsupported workflow")
			}
			definitions[oracleDefinitionIdentity{w.ProjectUID, "workflow", w.UID}] = db.ScopedDefinitionState{UID: w.UID, ProjectUID: w.ProjectUID, Kind: "workflow", Name: w.Name, Author: w.Author, CreatedAt: w.CreatedAt, Deleted: w.DeletedAt != nil, Workflow: &doc}
		}
		for _, j := range c.Fold.CronJobs {
			if !projects[j.ProjectUID] {
				continue
			}
			doc := j.Definition
			if doc.Version != 1 || doc.Kind != "job" {
				return out, fmt.Errorf("oracle unsupported job")
			}
			refs := []db.ScopedReference{}
			safe := true
			for _, field := range []struct {
				slot, id string
				workflow bool
			}{{"trigger.issue", doc.Trigger.IssueUID, false}, {"action.workflow", doc.Action.WorkflowUID, true}, {"issue.uid", func() string {
				if doc.Issue != nil {
					return doc.Issue.UID
				}
				return ""
			}(), false}, {"issue.parent", func() string {
				if doc.Issue != nil {
					return doc.Issue.ParentUID
				}
				return ""
			}(), false}} {
				if field.id == "" {
					continue
				}
				ref := db.ScopedReference{Kind: "definition", Slot: field.slot, Subject: db.ScopedEndpoint{ProjectUID: j.ProjectUID}, SubjectEntityUID: j.UID, Present: true, Revision: 1}
				if field.workflow {
					canonicalTarget, known := c.Fold.CronWorkflows[field.id]
					peer, ok := definitions[oracleDefinitionIdentity{canonicalTarget.ProjectUID, "workflow", field.id}]
					if !known || !ok || peer.Deleted {
						safe = false
						break
					}
					ref.Peer = &db.ScopedEndpoint{ProjectUID: peer.ProjectUID}
					ref.PeerEntityUID = peer.UID
					ref.Relation = "workflow"
				} else {
					peer, ok := issues[field.id]
					if !ok || peer.Deleted {
						safe = false
						break
					}
					ref.Peer = &db.ScopedEndpoint{ProjectUID: peer.ProjectUID, IssueUID: peer.UID}
					ref.Relation = "issue"
				}
				refs = append(refs, ref)
			}
			if !safe {
				continue
			}
			doc.Trigger.IssueUID = ""
			doc.Action.WorkflowUID = ""
			if doc.Issue != nil {
				policy := *doc.Issue
				policy.UID = ""
				policy.ParentUID = ""
				doc.Issue = &policy
			}
			definitions[oracleDefinitionIdentity{j.ProjectUID, "job", j.UID}] = db.ScopedDefinitionState{UID: j.UID, ProjectUID: j.ProjectUID, Kind: "job", Name: j.Name, Author: j.Author, CreatedAt: j.CreatedAt, Deleted: j.DeletedAt != nil, Job: &doc}
			out.References = append(out.References, refs...)
		}
		for _, v := range definitions {
			out.Definitions = append(out.Definitions, v)
		}
	}
	for _, p := range c.Creation {
		if !projects[p.ProjectUID] {
			continue
		}
		visible := false
		switch p.Kind {
		case "project":
			visible = p.EntityUID == p.ProjectUID
		case "issue":
			v, ok := issues[p.EntityUID]
			visible = ok && v.ProjectUID == p.ProjectUID
		case "comment":
			v, ok := comments[p.EntityUID]
			visible = ok && v.ProjectUID == p.ProjectUID
		case "cron_job":
			_, visible = definitions[oracleDefinitionIdentity{p.ProjectUID, "job", p.EntityUID}]
		case "cron_workflow":
			_, visible = definitions[oracleDefinitionIdentity{p.ProjectUID, "workflow", p.EntityUID}]
		default:
			return out, fmt.Errorf("oracle unsupported provenance %s", p.Kind)
		}
		if visible {
			p.Signatures = nil
			out.Provenance = append(out.Provenance, p)
		}
	}
	for _, a := range c.Artifacts {
		i, ok := issues[a.IssueUID]
		if ok && !i.Deleted && i.ProjectUID == a.ProjectUID && a.InputHash == embedding.ArtifactInputHash(embedding.EmbedText(i.Title, i.Body)) {
			if err := embedding.ValidateArtifactManifest(a); err != nil {
				return out, err
			}
			out.Artifacts = append(out.Artifacts, a)
		}
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].UID < out.Projects[j].UID })
	sort.Slice(out.Issues, func(i, j int) bool {
		if out.Issues[i].ProjectUID != out.Issues[j].ProjectUID {
			return out.Issues[i].ProjectUID < out.Issues[j].ProjectUID
		}
		return out.Issues[i].UID < out.Issues[j].UID
	})
	sort.Slice(out.Comments, func(i, j int) bool {
		if out.Comments[i].ProjectUID != out.Comments[j].ProjectUID {
			return out.Comments[i].ProjectUID < out.Comments[j].ProjectUID
		}
		return out.Comments[i].UID < out.Comments[j].UID
	})
	sort.Slice(out.Labels, func(i, j int) bool {
		a, b := out.Labels[i], out.Labels[j]
		return a.ProjectUID+":"+a.IssueUID+":"+a.Label < b.ProjectUID+":"+b.IssueUID+":"+b.Label
	})
	sort.Slice(out.Definitions, func(i, j int) bool {
		a, b := out.Definitions[i], out.Definitions[j]
		if a.ProjectUID != b.ProjectUID {
			return a.ProjectUID < b.ProjectUID
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.UID < b.UID
	})
	sort.Slice(out.References, func(i, j int) bool {
		a, b := out.References[i], out.References[j]
		return a.Subject.ProjectUID+":"+a.Subject.IssueUID+":"+a.SubjectEntityUID+":"+a.Slot < b.Subject.ProjectUID+":"+b.Subject.IssueUID+":"+b.SubjectEntityUID+":"+b.Slot
	})
	sort.Slice(out.Provenance, func(i, j int) bool {
		a, b := out.Provenance[i], out.Provenance[j]
		return a.ProjectUID+":"+a.Kind+":"+a.EntityUID < b.ProjectUID+":"+b.Kind+":"+b.EntityUID
	})
	sort.Slice(out.Artifacts, func(i, j int) bool {
		a, b := out.Artifacts[i], out.Artifacts[j]
		return a.ProjectUID+":"+a.Digest < b.ProjectUID+":"+b.Digest
	})
	// Own the test result too; decoding starts from fresh storage.
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	var copy db.ScopedCollections
	err = json.Unmarshal(raw, &copy)
	return copy, err
}
