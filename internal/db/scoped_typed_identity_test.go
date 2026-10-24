package db

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

// Native PutCronJob/PutCronWorkflow accept both typed objects with one UID.
// Scoped backup and signatures must preserve those ordinary native states.
func TestScopedTypedIdentityCollectionCollision(t *testing.T) {
	p, entity := verificationID(1), verificationID(40)
	job := ScopedDefinitionState{ProjectUID: p, UID: entity, Kind: "job", Job: &cron.JobDefinition{Version: 1, Kind: "job"}}
	workflow := ScopedDefinitionState{ProjectUID: p, UID: entity, Kind: "workflow", Workflow: &cron.WorkflowDefinition{Version: 1}}
	collections := verificationCollections()
	collections.Definitions = []ScopedDefinitionState{job, workflow}
	require.NoError(t, collections.Validate(), "typed namespaces may coexist in one project")
	collections.Definitions = append(collections.Definitions, job)
	require.Error(t, collections.Validate(), "same typed identity is still unique")
}

func TestScopedTypedIdentityEffectKeys(t *testing.T) {
	p, entity := verificationID(1), verificationID(40)
	job := ScopedDefinitionState{ProjectUID: p, UID: entity, Kind: "job", Job: &cron.JobDefinition{Version: 1, Kind: "job"}}
	workflow := ScopedDefinitionState{ProjectUID: p, UID: entity, Kind: "workflow", Workflow: &cron.WorkflowDefinition{Version: 1}}
	for _, definition := range []ScopedDefinitionState{job, workflow} {
		key := p + ":" + definition.Kind + ":" + entity
		replacement := ScopedEffect{Collection: "definitions", Key: key, Revision: 1, Action: "replace", Definition: &definition}
		project, err := ScopedEffectProjectUID(replacement)
		require.NoError(t, err)
		require.Equal(t, p, project)
		removal := ScopedEffect{Collection: "definitions", Key: key, Revision: 2, Action: "remove"}
		_, err = ScopedBatchComponentDigests([]ScopedEffect{replacement})
		require.NoError(t, err)
		_, err = ScopedEffectProjectUID(removal)
		require.NoError(t, err)
		replacement.Key = p + ":job:" + entity
		if definition.Kind == "workflow" {
			_, err = ScopedEffectProjectUID(replacement)
			require.Error(t, err, "typed key must match body")
		}
	}
	for _, key := range []string{p + ":" + entity, p + ":future:" + entity, p + ":job:" + entity + ":extra", p + ":job:alias"} {
		_, err := ScopedEffectProjectUID(ScopedEffect{Collection: "definitions", Key: key, Revision: 1, Action: "remove"})
		require.Error(t, err, "old/untyped or malformed removal must fail")
	}
}

func TestScopedTypedIdentityHelpers(t *testing.T) {
	p, entity := verificationID(1), verificationID(40)
	for _, kind := range []string{"job", "workflow"} {
		key, err := ScopedDefinitionKey(p, kind, entity)
		require.NoError(t, err)
		require.Equal(t, p+":"+kind+":"+entity, key)
	}
	for _, kind := range []string{"", "future", "job:workflow"} {
		_, err := ScopedDefinitionKey(p, kind, entity)
		require.Error(t, err)
	}
	_, err := ScopedDefinitionKey("alias", "job", entity)
	require.Error(t, err)
	_, err = ScopedDefinitionKey(p, "job", "alias")
	require.Error(t, err)
	for _, tc := range []struct{ command, subject, predicate, want string }{
		{"patchProjectMetadata", "project", "metadata", "project.metadata"},
		{"patchIssueMetadata", "issue", "metadata", "issue.metadata"},
		{"editIssue", "issue", "parent", "issue.parent"},
		{"closeIssue", "issue", "close", "issue.close"},
		{"replaceCronJob", "job", "definition", "job.definition"},
		{"replaceCronWorkflow", "workflow", "definition", "workflow.definition"},
	} {
		preparation := ScopedMutationPreparation{Kind: tc.command, Subject: ScopedMutationSubject{Kind: tc.subject, ProjectUID: p}, Predicate: tc.predicate}
		switch tc.subject {
		case "issue":
			preparation.Subject.IssueUID = entity
		case "job", "workflow":
			preparation.Subject.DefinitionUID = entity
		}
		if tc.predicate == "metadata" {
			preparation.MetadataGuard = &ScopedMetadataPredicate{Key: "example", IfAbsent: true}
		}
		got, err := ScopedPredicateStorageKind(preparation)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
		preparation.Predicate = "future"
		_, err = ScopedPredicateStorageKind(preparation)
		require.Error(t, err)
	}
}
