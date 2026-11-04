CREATE OR REPLACE FUNCTION rebuild_issue_search(p_issue_id BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_title TEXT;
  v_body TEXT;
  v_comments TEXT;
BEGIN
  -- Cascade guard: when the parent issue row is already vanishing the
  -- rebuild must skip. DELETE on issues cascades through comments first,
  -- firing comment-delete triggers; by the time the last one fires the
  -- issue row may already be gone, and re-inserting a search row would
  -- only be cascade-deleted moments later.
  IF NOT EXISTS (SELECT 1 FROM issues WHERE id = p_issue_id) THEN
    RETURN;
  END IF;
  SELECT title, COALESCE(body, '') INTO v_title, v_body FROM issues WHERE id = p_issue_id;
  SELECT COALESCE(string_agg(body, ' ' ORDER BY id), '')
    INTO v_comments
    FROM comments WHERE issue_id = p_issue_id;
  INSERT INTO issues_search (issue_id, tsv)
    VALUES (p_issue_id,
      to_tsvector('kata_simple_unaccent',
        coalesce(v_title,'') || ' ' || coalesce(v_body,'') || ' ' || coalesce(v_comments,'')))
    ON CONFLICT (issue_id) DO UPDATE SET tsv = EXCLUDED.tsv;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_issue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.issue_id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.issue_id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(OLD.issue_id);
  RETURN NULL;
END $$;

CREATE TRIGGER issues_search_after_issue_insert
  AFTER INSERT ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_issue_update
  AFTER UPDATE OF title, body ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_comment_insert
  AFTER INSERT ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_insert();

CREATE TRIGGER issues_search_after_comment_update
  AFTER UPDATE OF body ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_update();

CREATE TRIGGER issues_search_after_comment_delete
  AFTER DELETE ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_delete();
