package db

import (
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// ScopedDefinitionKey preserves native job/workflow namespaces, including when
// both entity types have the same project and UID. No global registry is needed.
func ScopedDefinitionKey(projectUID, kind, definitionUID string) (string, error) {
	if !uid.Valid(projectUID) || !uid.Valid(definitionUID) || (kind != "job" && kind != "workflow") {
		return "", errors.New("invalid typed scoped definition identity")
	}
	return projectUID + ":" + kind + ":" + definitionUID, nil
}

// ScopedPredicateStorageKind qualifies the persisted private counter identity.
// The authenticated preparation's wire predicate keeps its native meaning.
func ScopedPredicateStorageKind(preparation ScopedMutationPreparation) (string, error) {
	if err := preparation.Validate(); err != nil {
		return "", err
	}
	return preparation.Subject.Kind + "." + preparation.Predicate, nil
}
