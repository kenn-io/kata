-- Optional semantic-search state. Every row is derived from core issue data
-- and omitted from portable exports. The schema bootstrap applies this asset
-- only when pgvector 0.7 or later is available in public.
CREATE TABLE issue_vector_mirror (
  issue_uid        TEXT PRIMARY KEY,
  project_uid      TEXT NOT NULL,
  content          TEXT NOT NULL,
  content_revision BIGINT NOT NULL,
  embed_gen        TEXT
);

CREATE TABLE issue_vector_generations (
  ordinal    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  gen_key    TEXT NOT NULL UNIQUE,
  model      TEXT NOT NULL,
  dimensions INTEGER NOT NULL CHECK(dimensions > 0),
  state      TEXT NOT NULL CHECK(state IN ('building','active','retired'))
);

CREATE TABLE issue_vector_stamps (
  gen_key  TEXT NOT NULL REFERENCES issue_vector_generations(gen_key) ON DELETE CASCADE,
  issue_uid TEXT NOT NULL REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE,
  revision BIGINT NOT NULL,
  PRIMARY KEY(gen_key, issue_uid)
);

CREATE TABLE issue_vector_chunks (
  gen_key    TEXT NOT NULL REFERENCES issue_vector_generations(gen_key) ON DELETE CASCADE,
  issue_uid  TEXT NOT NULL REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE,
  chunk_index INTEGER NOT NULL,
  embedding  public.halfvec NOT NULL,
  PRIMARY KEY(gen_key, issue_uid, chunk_index)
);
CREATE INDEX idx_issue_vector_chunks_generation ON issue_vector_chunks(gen_key);
