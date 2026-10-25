package db

import (
	"errors"
	"slices"

	"go.kenn.io/kata/internal/embedding"
)

type ScopedOwnerFence struct {
	View   ScopedViewDescriptor
	Cut    ScopedCut
	Owners []ScopedProjectExport
}

type ScopedMaterialization struct {
	Fence       ScopedOwnerFence
	Collections ScopedCollections
}

// ScopedChangedProjects includes both endpoints of changed references. Removed
// slots retain their peer only in the prior inventory; unchanged slots must not
// cause unrelated project-filtered readers to refresh.
func ScopedChangedProjects(before, after ScopedCollections) ([]string, error) {
	effects, err := DiffScopedState(before, after, 1)
	if err != nil {
		return nil, err
	}
	prior, err := scopedCollectionRows(before)
	if err != nil {
		return nil, err
	}
	projects := map[string]bool{}
	for _, effect := range effects {
		project, err := ScopedEffectProjectUID(effect)
		if err != nil {
			return nil, err
		}
		projects[project] = true
		if effect.Collection != "references" {
			continue
		}
		for _, reference := range []*ScopedReference{prior["references/"+effect.Key].Reference, effect.Reference} {
			if reference == nil || !reference.Present {
				continue
			}
			if reference.Peer != nil {
				projects[reference.Peer.ProjectUID] = true
			}
			if reference.Close != nil {
				projects[reference.Close.PeerProjectUID] = true
			}
		}
	}
	keys := make([]string, 0, len(projects))
	for project := range projects {
		keys = append(keys, project)
	}
	slices.Sort(keys)
	return keys, nil
}

func ScopedAssertionJSON(value any) ([]byte, error) {
	return scopedCanonicalJSON(value)
}

func ScopedCollectionsFromInventory(effects []ScopedEffect) (ScopedCollections, error) {
	c := ScopedCollections{Projects: []ScopedProjectState{}, Issues: []ScopedIssueState{}, Comments: []ScopedCommentState{}, Labels: []ScopedLabelState{}, Definitions: []ScopedDefinitionState{}, References: []ScopedReference{}, Provenance: []ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
	for _, e := range effects {
		if e.Action == "remove" {
			continue
		}
		switch e.Collection {
		case "projects":
			c.Projects = append(c.Projects, *e.Project)
		case "issues":
			c.Issues = append(c.Issues, *e.Issue)
		case "comments":
			c.Comments = append(c.Comments, *e.Comment)
		case "labels":
			c.Labels = append(c.Labels, *e.Label)
		case "definitions":
			c.Definitions = append(c.Definitions, *e.Definition)
		case "references":
			c.References = append(c.References, *e.Reference)
		case "provenance":
			c.Provenance = append(c.Provenance, *e.Provenance)
		case "artifacts":
			c.Artifacts = append(c.Artifacts, *e.Artifact)
		default:
			return c, errors.New("unknown materialized scoped collection")
		}
	}
	return c, c.Validate()
}

func ScopedIssueAssertion(issue ScopedIssueState) ([]byte, error) {
	issue.CreatedAt = issue.CreatedAt.UTC()
	if issue.AssignmentExpiresOn != nil {
		stamp := issue.AssignmentExpiresOn.UTC()
		issue.AssignmentExpiresOn = &stamp
	}
	return scopedCanonicalJSON(issue)
}
