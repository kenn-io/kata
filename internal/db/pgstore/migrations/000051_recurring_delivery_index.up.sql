CREATE FUNCTION kata_occurrence_key(occurrence jsonb) RETURNS text
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
  version_text text;
  stamp text;
  parts text[];
  year_number integer;
  offset_seconds integer := 0;
  seconds bigint;
  fraction text;
BEGIN
  IF jsonb_typeof(occurrence) IS DISTINCT FROM 'object' OR jsonb_typeof(occurrence->'version') IS DISTINCT FROM 'number' THEN
    RETURN NULL;
  END IF;
  version_text := occurrence->>'version';
  IF version_text !~ '^-?(0|[1-9][0-9]*)$' THEN RETURN NULL; END IF;
  IF version_text::bigint > 1 THEN RETURN '~'; END IF;
  IF version_text::bigint <> 1 OR jsonb_typeof(occurrence->'scheduled_at') IS DISTINCT FROM 'string' THEN RETURN NULL; END IF;
  stamp := occurrence->>'scheduled_at';
  parts := regexp_match(stamp, '^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:[.]([0-9]+))?(Z|[+-][0-9]{2}:[0-9]{2})$');
  IF parts IS NULL OR parts[4]::integer > 23 OR parts[5]::integer > 59 OR parts[6]::integer > 59 THEN RETURN NULL; END IF;
  year_number := parts[1]::integer;
  IF parts[8] <> 'Z' THEN
    IF substring(parts[8] from 2 for 2)::integer >= 24 OR substring(parts[8] from 5 for 2)::integer >= 60 THEN RETURN NULL; END IF;
    offset_seconds := (substring(parts[8] from 2 for 2)::integer * 60 + substring(parts[8] from 5 for 2)::integer) * 60;
    IF left(parts[8], 1) = '-' THEN offset_seconds := -offset_seconds; END IF;
  END IF;
  seconds := extract(epoch FROM make_timestamp(CASE WHEN year_number = 0 THEN -1 ELSE year_number END, parts[2]::integer, parts[3]::integer, parts[4]::integer, parts[5]::integer, parts[6]::double precision))::bigint - offset_seconds;
  fraction := rpad(left(coalesce(parts[7], ''), 9), 9, '0');
  IF seconds = -62135596800 AND fraction = '000000000' THEN RETURN NULL; END IF;
  RETURN lpad((seconds + 62167478400)::text, 12, '0') || '.' || fraction;
EXCEPTION WHEN OTHERS THEN
  RETURN NULL;
END
$$;

CREATE INDEX idx_issues_recurring_delivery ON issues (
  ((metadata::jsonb -> 'cron.occurrence' ->> 'job_uid') COLLATE "C"),
  (kata_occurrence_key(metadata::jsonb -> 'cron.occurrence') COLLATE "C") DESC,
  uid COLLATE "C" DESC
) WHERE deleted_at IS NULL AND kata_occurrence_key(metadata::jsonb -> 'cron.occurrence') IS NOT NULL;
