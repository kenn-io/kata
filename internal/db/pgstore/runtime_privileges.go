package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var canonicalSequenceNames = strings.Fields(`
projects_id_seq project_aliases_id_seq recurrences_id_seq issues_id_seq
comments_id_seq links_id_seq events_id_seq api_tokens_id_seq purge_log_id_seq
project_purge_log_id_seq issue_sync_bindings_id_seq federation_quarantine_id_seq
federation_enrollments_id_seq issue_claims_id_seq pending_claim_requests_id_seq
import_mappings_id_seq issue_vector_generations_ordinal_seq`)

func (s *Store) validateRuntimePrivileges(ctx context.Context) error {
	var allowed bool
	if err := s.QueryRowContext(ctx,
		`SELECT pg_catalog.has_schema_privilege(current_user, $1, 'USAGE')`,
		s.schema,
	).Scan(&allowed); err != nil {
		return fmt.Errorf("inspect postgres schema %q runtime USAGE privilege: %w", s.schema, err)
	}
	if !allowed {
		return fmt.Errorf("postgres runtime role lacks USAGE privilege on schema %q", s.schema)
	}
	if err := s.validateRuntimeTablePrivileges(ctx); err != nil {
		return err
	}
	if err := s.validateRuntimeSequencePrivileges(ctx); err != nil {
		return err
	}
	return s.validateRuntimeFunctionPrivileges(ctx)
}

func (s *Store) validateRuntimeTablePrivileges(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT c.relname,
		       pg_catalog.has_table_privilege(current_user, c.oid, 'SELECT'),
		       pg_catalog.has_table_privilege(current_user, c.oid, 'INSERT'),
		       pg_catalog.has_table_privilege(current_user, c.oid, 'UPDATE'),
		       pg_catalog.has_table_privilege(current_user, c.oid, 'DELETE')
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		 ORDER BY c.relname`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q runtime table privileges: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table string
		var selectAllowed, insertAllowed, updateAllowed, deleteAllowed bool
		if err := rows.Scan(&table, &selectAllowed, &insertAllowed, &updateAllowed, &deleteAllowed); err != nil {
			return fmt.Errorf("scan postgres schema %q runtime table privileges: %w", s.schema, err)
		}
		if _, canonical := canonicalTableNames[table]; !canonical {
			continue
		}
		for _, check := range []struct {
			privilege string
			granted   bool
		}{
			{privilege: "SELECT", granted: selectAllowed},
			{privilege: "INSERT", granted: insertAllowed},
			{privilege: "UPDATE", granted: updateAllowed},
			{privilege: "DELETE", granted: deleteAllowed},
		} {
			if !check.granted {
				return fmt.Errorf(
					"postgres runtime role lacks %s privilege on table %q",
					check.privilege, s.schema+"."+table,
				)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q runtime table privileges: %w", s.schema, err)
	}
	return nil
}

func (s *Store) validateRuntimeSequencePrivileges(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT c.relname,
		       pg_catalog.has_sequence_privilege(current_user, c.oid, 'USAGE'),
		       pg_catalog.has_sequence_privilege(current_user, c.oid, 'SELECT'),
		       pg_catalog.has_sequence_privilege(current_user, c.oid, 'UPDATE')
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind = 'S'
		 ORDER BY c.relname`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q runtime sequence privileges: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	found := make(map[string]struct{}, len(canonicalSequenceNames))
	for rows.Next() {
		var sequence string
		var usageAllowed, selectAllowed, updateAllowed bool
		if err := rows.Scan(&sequence, &usageAllowed, &selectAllowed, &updateAllowed); err != nil {
			return fmt.Errorf("scan postgres schema %q runtime sequence privileges: %w", s.schema, err)
		}
		if !isCanonicalSequence(sequence) {
			continue
		}
		found[sequence] = struct{}{}
		for _, check := range []struct {
			privilege string
			granted   bool
		}{
			{privilege: "USAGE", granted: usageAllowed},
			{privilege: "SELECT", granted: selectAllowed},
			{privilege: "UPDATE", granted: updateAllowed},
		} {
			if !check.granted {
				return fmt.Errorf(
					"postgres runtime role lacks %s privilege on sequence %q",
					check.privilege, s.schema+"."+sequence,
				)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q runtime sequence privileges: %w", s.schema, err)
	}
	for _, sequence := range canonicalSequenceNames {
		if _, ok := found[sequence]; !ok {
			return fmt.Errorf("postgres schema %q is missing canonical sequence %q", s.schema, sequence)
		}
	}
	return nil
}

func isCanonicalSequence(name string) bool {
	for _, canonical := range canonicalSequenceNames {
		if name == canonical {
			return true
		}
	}
	return false
}

func (s *Store) validateRuntimeFunctionPrivileges(ctx context.Context) error {
	var allowed bool
	err := s.QueryRowContext(ctx, `
		SELECT pg_catalog.has_function_privilege(current_user, p.oid, 'EXECUTE')
		  FROM pg_catalog.pg_proc p
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = $1
		   AND p.proname = 'rewrite_project_uid_for_adoption'
		   AND pg_catalog.oidvectortypes(p.proargtypes) = 'bigint, text'`, s.schema).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"postgres schema %q is missing canonical function %q",
			s.schema, "rewrite_project_uid_for_adoption(bigint,text)",
		)
	}
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q runtime function privileges: %w", s.schema, err)
	}
	if !allowed {
		return fmt.Errorf(
			"postgres runtime role lacks EXECUTE privilege on function %q",
			s.schema+".rewrite_project_uid_for_adoption(bigint,text)",
		)
	}
	return nil
}
