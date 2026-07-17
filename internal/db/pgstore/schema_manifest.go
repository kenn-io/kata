package pgstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	canonicalColumnFingerprint     = "789e6ae6066ad24cfdc8f918275b90a26725c7fd6d1e942a0f76dc89ec15d1b9"
	canonicalConstraintFingerprint = "3867e088c3ab4395d517d0788ffa658f589164b6b6bd68541fde3d648cd29363"
	canonicalIndexFingerprint      = "3a9b60f05ac5c8064a0e61d867fed7ae3c9e0fa7b345bd3236e3300273801dac"
	canonicalTriggerFingerprint    = "34f80f8e2b61c6cb1705bb8effb1548164e91f181fb58b68cf929ac90a190154"
	canonicalFunctionFingerprint   = "f96c6a857ec7bc7c3e5c1cebdd3d7b795547f4399f8f65ea0aa9befa98681741"
	canonicalTextSearchFingerprint = "1206eb613d5e4cf9c00c447998c337ece326819f8b3261668f7a8c685571db06"
)

var canonicalTableColumns = map[string]string{ //nolint:gosec // Catalog column names, not credential values.
	"api_tokens":               "id,token_hash,actor,name,created_at,last_used_at,revoked_at",
	"comments":                 "id,uid,issue_id,author,body,created_at",
	"events":                   "id,uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,related_issue_id,related_issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at",
	"federation_bindings":      "project_id,role,hub_url,hub_project_id,hub_project_uid,replay_horizon_event_id,pull_cursor_event_id,push_enabled,push_cursor_event_id,bound_actor,allow_insecure,enabled,created_at,updated_at,last_sync_at",
	"federation_enrollments":   "id,token_hash,spoke_instance_uid,project_id,capabilities,bound_actor,allow_adoption_snapshot_authors,adoption_baseline_open,adoption_baseline_next_source_event_id,adoption_baseline_end_source_event_id,created_at,updated_at,revoked_at",
	"federation_quarantine":    "id,project_id,direction,first_event_id,last_event_id,event_uids,error,created_at,skipped_at,skipped_by,skip_reason",
	"federation_sync_status":   "project_id,last_pull_started_at,last_pull_success_at,last_push_started_at,last_push_success_at,last_error_at,last_error,last_reset_at",
	"import_mappings":          "id,source,external_id,object_type,project_id,issue_id,comment_id,link_id,label,source_updated_at,imported_at",
	"issue_claims":             "id,claim_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,purpose,claim_kind,acquired_at,expires_at,released_at,release_reason,revision,updated_at",
	"issue_labels":             "issue_id,label,author,created_at",
	"issue_sync_bindings":      "id,project_id,provider,source_key,remote_id,display_name,config_json,enabled,interval_seconds,last_cursor_at,created_at,updated_at",
	"issue_sync_status":        "binding_id,project_id,sync_started_at,last_attempt_at,last_success_at,last_error_at,last_error,last_created,last_updated,last_unchanged,last_comments",
	"issue_vector_chunks":      "gen_key,issue_uid,chunk_index,embedding",
	"issue_vector_generations": "ordinal,gen_key,model,dimensions,state",
	"issue_vector_mirror":      "issue_uid,project_uid,content,content_revision,embed_gen",
	"issue_vector_stamps":      "gen_key,issue_uid,revision",
	"issues":                   "id,uid,project_id,short_id,title,body,status,closed_reason,owner,priority,author,created_at,updated_at,closed_at,deleted_at,metadata,revision,content_revision,recurrence_id,occurrence_key",
	"issues_search":            "issue_id,tsv",
	"links":                    "id,from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author,created_at",
	"meta":                     "key,value",
	"pending_claim_requests":   "id,request_uid,project_id,issue_id,issue_uid,holder,holder_instance_uid,client_kind,claim_kind,ttl_seconds,purpose,requested_at,last_attempt_at,last_error,rejected_at,resolved_at",
	"project_aliases":          "id,project_id,alias_identity,alias_kind,created_at",
	"project_purge_log":        "id,uid,origin_instance_uid,project_id,project_uid,project_name,issue_count,event_count,alias_count,comment_count,link_count,label_count,claim_count,pending_claim_request_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,actor,reason,purged_at",
	"projects":                 "id,uid,name,created_at,deleted_at,metadata,revision",
	"purge_log":                "id,uid,origin_instance_uid,project_id,purged_issue_id,issue_uid,project_uid,project_name,issue_title,issue_author,comment_count,link_count,label_count,event_count,events_deleted_min_id,events_deleted_max_id,purge_reset_after_event_id,short_id,actor,reason,purged_at",
	"recurrences":              "id,uid,project_id,rrule,dtstart,timezone,template_title,template_body,template_owner,template_priority,template_labels,template_metadata,next_occurrence_key,last_materialized_uid,author,revision,created_at,updated_at,deleted_at",
}

var canonicalIndexes = strings.Fields(`
idx_projects_active idx_project_aliases_project recurrences_project
idx_issues_project_status_updated idx_issues_project_updated idx_issues_owner
uniq_issues_project_short_id issues_recurrence_occurrence_uniq idx_comments_issue
uniq_one_parent_per_child idx_links_from idx_links_to idx_links_from_uid idx_links_to_uid
idx_issue_labels_label idx_events_project idx_events_issue idx_events_related
idx_events_issue_uid idx_events_related_issue_uid idx_events_origin_instance
idx_events_origin_project_id idx_events_hlc idx_events_content_hash idx_events_idempotency
idx_purge_log_reset idx_purge_log_project_reset idx_purge_log_issue idx_purge_log_issue_uid
idx_purge_log_project_uid idx_purge_log_origin_instance idx_purge_log_short_id
idx_project_purge_log_reset idx_project_purge_log_project_reset idx_federation_bindings_role_enabled
idx_issue_sync_bindings_due idx_issue_sync_status_project idx_issue_sync_status_due
uniq_federation_quarantine_active idx_federation_enrollments_scope idx_federation_enrollments_spoke
uniq_issue_claims_live_issue idx_issue_claims_project_issue idx_issue_claims_timed_expiry
uniq_pending_claim_active idx_issues_search_tsv idx_import_mappings_issue
idx_import_mappings_comment idx_import_mappings_link idx_issue_vector_chunks_generation`)

var canonicalTriggers = strings.Fields(`
trg_links_uid_consistency_insert trg_links_uid_consistency_update
trg_projects_uid_immutable trg_issues_uid_immutable trg_issue_sync_binding_immutable
issues_search_after_issue_insert issues_search_after_issue_update
issues_search_after_comment_insert issues_search_after_comment_update issues_search_after_comment_delete`)

var canonicalFunctions = strings.Fields(`
enforce_links_uid_consistency()
enforce_uid_immutable()
rewrite_project_uid_for_adoption(bigint,text)
enforce_issue_sync_binding_immutable()
rebuild_issue_search(bigint)
issues_search_trigger_on_issue()
issues_search_trigger_on_comment_insert()
issues_search_trigger_on_comment_update()
issues_search_trigger_on_comment_delete()`)

func (s *Store) validateSchemaManifest(ctx context.Context) error {
	if err := s.validateCanonicalColumns(ctx); err != nil {
		return err
	}
	if err := s.validateCanonicalConstraints(ctx); err != nil {
		return err
	}
	if err := s.validateCanonicalNamedObjects(ctx, "index", canonicalIndexes, `
		SELECT c.relname
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind = 'i'`); err != nil {
		return err
	}
	if err := s.validateCanonicalIndexes(ctx); err != nil {
		return err
	}
	if err := s.validateCanonicalNamedObjects(ctx, "trigger", canonicalTriggers, `
		SELECT t.tgname
		  FROM pg_catalog.pg_trigger t
		  JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND NOT t.tgisinternal`); err != nil {
		return err
	}
	if err := s.validateCanonicalTriggers(ctx); err != nil {
		return err
	}
	if err := s.validateCanonicalFunctions(ctx); err != nil {
		return err
	}
	return s.validateCanonicalTextSearch(ctx)
}

func (s *Store) validateCanonicalColumns(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT table_name, column_name, udt_schema || '.' || udt_name, is_nullable
		  FROM information_schema.columns
		 WHERE table_schema = $1`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q columns: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	actual := make(map[string]map[string]struct{}, len(canonicalTableColumns))
	var records []string
	for rows.Next() {
		var table, column, dataType, nullable string
		if err := rows.Scan(&table, &column, &dataType, &nullable); err != nil {
			return fmt.Errorf("scan postgres schema %q columns: %w", s.schema, err)
		}
		if _, canonical := canonicalTableColumns[table]; !canonical {
			continue
		}
		if actual[table] == nil {
			actual[table] = make(map[string]struct{})
		}
		actual[table][column] = struct{}{}
		records = append(records, strings.Join([]string{"COLUMN", table, column, dataType, nullable}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q columns: %w", s.schema, err)
	}
	for table, columns := range canonicalTableColumns {
		expected := strings.Split(columns, ",")
		if len(actual[table]) != len(expected) {
			for _, column := range expected {
				if _, ok := actual[table][column]; !ok {
					return fmt.Errorf("postgres schema %q is missing canonical column %q", s.schema, table+"."+column)
				}
			}
			return fmt.Errorf("postgres schema %q relation %q has unexpected columns", s.schema, table)
		}
		for _, column := range expected {
			if _, ok := actual[table][column]; !ok {
				return fmt.Errorf("postgres schema %q is missing canonical column %q", s.schema, table+"."+column)
			}
		}
	}
	return validateCatalogFingerprint(s.schema, "column", records, canonicalColumnFingerprint)
}

func (s *Store) validateCanonicalConstraints(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT c.relname, con.conname, con.contype::text,
		       pg_catalog.pg_get_constraintdef(con.oid, true)
		  FROM pg_catalog.pg_constraint con
		  JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND con.contype <> 'n'`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q constraints: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var records []string
	for rows.Next() {
		var table, name, constraintType, definition string
		if err := rows.Scan(&table, &name, &constraintType, &definition); err != nil {
			return fmt.Errorf("scan postgres schema %q constraints: %w", s.schema, err)
		}
		records = append(records, strings.Join([]string{"CONSTRAINT", table, name, constraintType, definition}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q constraints: %w", s.schema, err)
	}
	return validateCatalogFingerprint(s.schema, "constraint", records, canonicalConstraintFingerprint)
}

func (s *Store) validateCanonicalIndexes(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT idx.relname, tbl.relname, i.indisvalid, i.indisready,
		       pg_catalog.pg_get_indexdef(i.indexrelid)
		  FROM pg_catalog.pg_index i
		  JOIN pg_catalog.pg_class idx ON idx.oid = i.indexrelid
		  JOIN pg_catalog.pg_class tbl ON tbl.oid = i.indrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid = idx.relnamespace
		 WHERE n.nspname = $1`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q index definitions: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var records []string
	for rows.Next() {
		var name, table, definition string
		var valid, ready bool
		if err := rows.Scan(&name, &table, &valid, &ready, &definition); err != nil {
			return fmt.Errorf("scan postgres schema %q index definition: %w", s.schema, err)
		}
		definition = strings.ReplaceAll(definition, s.schema+".", "")
		records = append(records, strings.Join([]string{
			"INDEX", name, table, strconv.FormatBool(valid), strconv.FormatBool(ready), definition,
		}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q index definitions: %w", s.schema, err)
	}
	return validateCatalogFingerprint(s.schema, "index", records, canonicalIndexFingerprint)
}

func (s *Store) validateCanonicalTriggers(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT t.tgname, c.relname, t.tgenabled::text,
		       pg_catalog.pg_get_triggerdef(t.oid, true)
		  FROM pg_catalog.pg_trigger t
		  JOIN pg_catalog.pg_class c ON c.oid = t.tgrelid
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND NOT t.tgisinternal`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q trigger definitions: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var records []string
	for rows.Next() {
		var name, table, enabled, definition string
		if err := rows.Scan(&name, &table, &enabled, &definition); err != nil {
			return fmt.Errorf("scan postgres schema %q trigger definition: %w", s.schema, err)
		}
		definition = strings.ReplaceAll(definition, s.schema+".", "")
		records = append(records, strings.Join([]string{"TRIGGER", name, table, enabled, definition}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q trigger definitions: %w", s.schema, err)
	}
	return validateCatalogFingerprint(s.schema, "trigger", records, canonicalTriggerFingerprint)
}

func validateCatalogFingerprint(schema, kind string, records []string, expected string) error {
	sort.Strings(records)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(records, "\n"))))
	if digest != expected {
		return fmt.Errorf(
			"postgres schema %q canonical %s definitions do not match (got %s, want %s)",
			schema, kind, digest, expected,
		)
	}
	return nil
}

func (s *Store) validateCanonicalNamedObjects(
	ctx context.Context,
	kind string,
	expected []string,
	query string,
) error {
	rows, err := s.QueryContext(ctx, query, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q %ss: %w", s.schema, kind, err)
	}
	defer func() { _ = rows.Close() }()
	actual := make(map[string]struct{}, len(expected))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan postgres schema %q %s: %w", s.schema, kind, err)
		}
		actual[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q %ss: %w", s.schema, kind, err)
	}
	for _, name := range expected {
		if _, ok := actual[name]; !ok {
			return fmt.Errorf("postgres schema %q is missing canonical %s %q", s.schema, kind, name)
		}
	}
	return nil
}

func (s *Store) validateCanonicalFunctions(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT p.proname, pg_catalog.oidvectortypes(p.proargtypes),
		       pg_catalog.pg_get_function_arguments(p.oid),
		       pg_catalog.pg_get_function_result(p.oid), l.lanname,
		       p.prokind::text, p.provolatile::text, p.proisstrict,
		       p.prosecdef, p.proleakproof, p.proparallel::text,
		       COALESCE(pg_catalog.array_to_string(p.proconfig, E'\x1f'), ''),
		       p.prosrc
		  FROM pg_catalog.pg_proc p
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		  JOIN pg_catalog.pg_language l ON l.oid = p.prolang
		 WHERE n.nspname = $1`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q functions: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	actual := make(map[string]struct{}, len(canonicalFunctions))
	var records []string
	for rows.Next() {
		var name, identityArguments, arguments, result, language string
		var kind, volatility, parallel, configuration, source string
		var strict, securityDefiner, leakproof bool
		if err := rows.Scan(
			&name, &identityArguments, &arguments, &result, &language,
			&kind, &volatility, &strict, &securityDefiner, &leakproof,
			&parallel, &configuration, &source,
		); err != nil {
			return fmt.Errorf("scan postgres schema %q function: %w", s.schema, err)
		}
		actual[name+"("+strings.ReplaceAll(identityArguments, " ", "")+")"] = struct{}{}
		configuration = normalizeFunctionConfiguration(configuration, s.schema)
		records = append(records, strings.Join([]string{
			"FUNCTION", name, identityArguments, arguments, result, language,
			kind, volatility, strconv.FormatBool(strict), strconv.FormatBool(securityDefiner),
			strconv.FormatBool(leakproof), parallel, configuration, source,
		}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q functions: %w", s.schema, err)
	}
	for _, name := range canonicalFunctions {
		if _, ok := actual[name]; !ok {
			return fmt.Errorf("postgres schema %q is missing canonical function %q", s.schema, name)
		}
	}
	return validateCatalogFingerprint(s.schema, "function", records, canonicalFunctionFingerprint)
}

func normalizeFunctionConfiguration(configuration, schema string) string {
	settings := strings.Split(configuration, "\x1f")
	for i, setting := range settings {
		name, value, ok := strings.Cut(setting, "=")
		if !ok || name != "search_path" {
			continue
		}
		entries := strings.Split(value, ",")
		for j, entry := range entries {
			if strings.TrimSpace(entry) == schema {
				entries[j] = strings.Replace(entry, schema, "<schema>", 1)
			}
		}
		settings[i] = name + "=" + strings.Join(entries, ",")
	}
	return strings.Join(settings, "\x1f")
}

func (s *Store) validateCanonicalTextSearch(ctx context.Context) error {
	rows, err := s.QueryContext(ctx, `
		SELECT c.cfgname, parser_ns.nspname || '.' || parser.prsname,
		       token.alias, mapping.mapseqno,
		       dictionary_ns.nspname || '.' || dictionary.dictname
		  FROM pg_catalog.pg_ts_config c
		  JOIN pg_catalog.pg_namespace config_ns ON config_ns.oid = c.cfgnamespace
		  JOIN pg_catalog.pg_ts_parser parser ON parser.oid = c.cfgparser
		  JOIN pg_catalog.pg_namespace parser_ns ON parser_ns.oid = parser.prsnamespace
		  JOIN pg_catalog.pg_ts_config_map mapping ON mapping.mapcfg = c.oid
		  JOIN LATERAL pg_catalog.ts_token_type(c.cfgparser) token
		    ON token.tokid = mapping.maptokentype
		  JOIN pg_catalog.pg_ts_dict dictionary ON dictionary.oid = mapping.mapdict
		  JOIN pg_catalog.pg_namespace dictionary_ns ON dictionary_ns.oid = dictionary.dictnamespace
		 WHERE config_ns.nspname = $1 AND c.cfgname = 'kata_simple_unaccent'`, s.schema)
	if err != nil {
		return fmt.Errorf("inspect postgres schema %q text search definitions: %w", s.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var records []string
	for rows.Next() {
		var config, parser, token, dictionary string
		var sequence int
		if err := rows.Scan(&config, &parser, &token, &sequence, &dictionary); err != nil {
			return fmt.Errorf("scan postgres schema %q text search definition: %w", s.schema, err)
		}
		records = append(records, strings.Join([]string{
			"TEXT_SEARCH", config, parser, token, strconv.Itoa(sequence), dictionary,
		}, "\x00"))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postgres schema %q text search definitions: %w", s.schema, err)
	}
	return validateCatalogFingerprint(s.schema, "text search", records, canonicalTextSearchFingerprint)
}
