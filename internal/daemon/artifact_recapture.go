package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/pagination"
)

const artifactRecapturePageSize = 32

// recaptureHubArtifacts rotates one bounded issue page through enabled hub
// projects. The index already has the vectors for these documents; this pass
// only fills the portable-artifact gap left by indexing before retention was
// introduced. It is best effort so a missing/corrupt old vector cannot stop
// current pending documents from being embedded.
func (r *Reconciler) recaptureHubArtifacts(ctx context.Context, key string, recipe embedding.ArtifactIdentity) error {
	if r.idx == nil {
		return nil
	}
	if _, ok := r.store.(db.EmbeddingArtifactStorage); !ok {
		return nil
	}
	if _, ok := r.store.(db.EmbeddingArtifactExporter); !ok {
		return nil
	}
	if r.recaptureKey != key {
		r.recaptureKey = key
		r.recaptureCursors = nil
		r.recaptureProjectCursor = ""
	}
	bindings, err := r.store.ListFederationBindings(ctx)
	if err != nil {
		slog.Warn("artifact recapture: list federation bindings failed", "err", err)
		return nil
	}
	self := r.store.InstanceUID()
	projects := make(map[string]db.Project)
	for _, binding := range bindings {
		if !binding.Enabled || binding.Role != db.FederationRoleHub {
			continue
		}
		project, err := r.store.ProjectByID(ctx, binding.ProjectID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
			continue
		}
		if err != nil {
			slog.Warn("artifact recapture: load hub project failed", "project_id", binding.ProjectID, "err", err)
			continue
		}
		producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
		if err != nil || producer == nil || producer.ProducerInstanceUID != self || !sameArtifactRecipe(producer.Recipe, recipe) {
			continue
		}
		projects[project.UID] = project
	}
	if len(projects) == 0 {
		return nil
	}
	uids := make([]string, 0, len(projects))
	for uid := range projects {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	projectUID := rotateArtifactProject(uids, r.recaptureProjectCursor)
	project := projects[projectUID]
	var after *pagination.Position
	if r.recaptureCursors != nil {
		after = r.recaptureCursors[projectUID]
	}
	issues, err := r.store.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: project.ID, OldestFirst: true, Limit: artifactRecapturePageSize, After: after,
	})
	if err != nil {
		slog.Warn("artifact recapture: list hub issues failed", "project_uid", projectUID, "err", err)
		return nil
	}
	if len(issues) == 0 {
		if r.recaptureCursors != nil {
			delete(r.recaptureCursors, projectUID)
		}
		r.recaptureProjectCursor = projectUID
		return nil
	}
	docs := make([]string, len(issues))
	for i, issue := range issues {
		docs[i] = issue.UID
	}
	if _, err := r.idx.RecaptureArtifacts(ctx, key, r.store, recipe, projectUID, docs); err != nil {
		slog.Warn("artifact recapture failed", "project_uid", projectUID, "err", err)
		return nil
	}
	r.recaptureProjectCursor = projectUID
	last := issues[len(issues)-1]
	if len(issues) < artifactRecapturePageSize {
		if r.recaptureCursors != nil {
			delete(r.recaptureCursors, projectUID)
		}
		return nil
	}
	if r.recaptureCursors == nil {
		r.recaptureCursors = make(map[string]*pagination.Position)
	}
	r.recaptureCursors[projectUID] = &pagination.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	return nil
}

func rotateArtifactProject(projects []string, after string) string {
	for _, project := range projects {
		if project > after {
			return project
		}
	}
	return projects[0]
}

func sameArtifactRecipe(a, b embedding.ArtifactIdentity) bool {
	a.ProjectUID, a.IssueUID, a.InputHash, a.ProducerInstanceUID = "", "", "", ""
	b.ProjectUID, b.IssueUID, b.InputHash, b.ProducerInstanceUID = "", "", "", ""
	return a == b
}
