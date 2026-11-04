-- Replace aggregate comment indexing while serving daemons and imports are stopped.
DROP TRIGGER issues_search_after_comment_insert ON comments;
DROP TRIGGER issues_search_after_comment_update ON comments;
DROP TRIGGER issues_search_after_comment_delete ON comments;
DROP FUNCTION issues_search_trigger_on_comment_insert();
DROP FUNCTION issues_search_trigger_on_comment_update();
DROP FUNCTION issues_search_trigger_on_comment_delete();
DROP TRIGGER issues_search_after_issue_insert ON issues;
DROP TRIGGER issues_search_after_issue_update ON issues;

CREATE OR REPLACE FUNCTION rebuild_issue_search(p_issue_id BIGINT) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO issues_search (issue_id, tsv)
    SELECT id, to_tsvector('kata_simple_unaccent', title || ' ' || coalesce(body, ''))
    FROM issues WHERE id = p_issue_id
    ON CONFLICT (issue_id) DO UPDATE SET tsv = EXCLUDED.tsv;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_issue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.id);
  RETURN NULL;
END $$;

CREATE TRIGGER issues_search_after_issue_insert
  AFTER INSERT ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();
CREATE TRIGGER issues_search_after_issue_update
  AFTER UPDATE OF title, body ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE INDEX idx_comments_search ON comments USING GIN (to_tsvector('kata_simple_unaccent', body));

UPDATE issues_search s SET tsv = to_tsvector('kata_simple_unaccent', i.title || ' ' || coalesce(i.body, ''))
  FROM issues i WHERE i.id = s.issue_id;
ANALYZE comments;
ANALYZE issues_search;
