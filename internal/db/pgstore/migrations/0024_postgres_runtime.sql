-- Normalize version-23 installations created before the Postgres runtime
-- invariants were fully specified. Fresh installations also run this asset so
-- the baseline and upgrade paths converge on the same extension location and
-- reserved audit project.

CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public;
DO $$
BEGIN
  IF (SELECT extnamespace FROM pg_extension WHERE extname = 'unaccent')
       <> (SELECT oid FROM pg_namespace WHERE nspname = 'public') THEN
    ALTER EXTENSION unaccent SET SCHEMA public;
  END IF;
END
$$;

ALTER TEXT SEARCH CONFIGURATION kata_simple_unaccent
  ALTER MAPPING FOR hword, hword_part, word
  WITH public.unaccent, pg_catalog.simple;

INSERT INTO projects(uid, name)
VALUES ('00000000000000000000000000', '.kata-system')
ON CONFLICT(name) DO NOTHING;
