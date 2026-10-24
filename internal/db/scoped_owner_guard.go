package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// SourceOwnerGuard is the literal native UnassignOwner expected-owner value.
// The operation-level pointer distinguishes unconditional V1 from guarded V2;
// ExpectedOwner may be empty and is never trimmed or made into an ABA token.
type SourceOwnerGuard struct {
	Subject       ScopedEndpoint `json:"subject"`
	ExpectedOwner string         `json:"expected_owner"`
}

func (g *SourceOwnerGuard) UnmarshalJSON(raw []byte) error {
	type plain SourceOwnerGuard
	var value plain
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	owner := bytes.TrimSpace(fields["expected_owner"])
	if len(owner) == 0 || owner[0] != '"' || value.Subject.Validate() != nil {
		return errors.New("incomplete source owner guard")
	}
	*g = SourceOwnerGuard(value)
	return nil
}

func validateSourceOperationVersion(op SourceOperation) error {
	switch op.Version {
	case 1:
		if op.ImportIntent != nil {
			return errors.New("version1 operation cannot contain import intent")
		}
		if op.OwnerGuard != nil {
			return errors.New("version1 operation cannot contain owner guard")
		}
	case 2:
		if op.ImportIntent != nil {
			return errors.New("version2 operation cannot contain import intent")
		}
		guard := op.OwnerGuard
		if op.CommandKind != "unassignIssue" || guard == nil || guard.Subject.Validate() != nil || len(op.Members) != 1 || len(op.Membership) != 1 || op.Members[0].ProjectUID != guard.Subject.ProjectUID || op.Membership[0].Endpoint != guard.Subject {
			return errors.New("version2 operation requires exact unassignment owner guard")
		}
	case 3:
		if op.CommandKind != "importIssues" || op.ImportIntent == nil || op.OwnerGuard != nil || len(op.Members) != 0 || len(op.Membership) != 0 || len(op.Expectations) != 0 {
			return errors.New("version3 requires an exclusive normalized import intent")
		}
		if err := op.ImportIntent.validate(op.Principal.AccountableActor); err != nil {
			return err
		}
	default:
		return errors.New("unsupported source operation version")
	}
	return nil
}

func (op SourceOperation) MarshalJSON() ([]byte, error) {
	if err := validateSourceOperationVersion(op); err != nil {
		return nil, err
	}
	if op.Version == 3 {
		return json.Marshal(struct {
			Version           int                 `json:"version"`
			UID               string              `json:"uid"`
			Digest            string              `json:"digest"`
			SourceInstanceUID string              `json:"source_instance_uid"`
			CommandKind       string              `json:"command_kind"`
			IdempotencyKey    string              `json:"idempotency_key"`
			Principal         CustodyPrincipal    `json:"principal"`
			ImportIntent      *ScopedImportIntent `json:"import_intent"`
		}{op.Version, op.UID, op.Digest, op.SourceInstanceUID, op.CommandKind, op.IdempotencyKey, op.Principal, op.ImportIntent})
	}
	// Field order and JSON v2 nil-array encoding remain the V1 transcript.
	type plain SourceOperation
	return json.Marshal(plain(op))
}

func (op *SourceOperation) UnmarshalJSON(raw []byte) error {
	type plain SourceOperation
	var value plain
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, present := fields["import_intent"]; present && value.Version != 3 {
		return errors.New("old source versions cannot contain import_intent")
	}
	if value.Version == 3 {
		for _, name := range []string{"members", "membership", "expectations", "owner_guard"} {
			if _, present := fields[name]; present {
				return errors.New("import intent contains a producer/guard arm")
			}
		}
	}
	if _, present := fields["owner_guard"]; value.Version == 1 && present {
		return errors.New("version1 operation cannot contain owner_guard, even null")
	}
	decoded := SourceOperation(value)
	if err := validateSourceOperationVersion(decoded); err != nil {
		return err
	}
	*op = decoded
	return nil
}
