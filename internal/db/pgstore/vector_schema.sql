-- Optional semantic-search state. Every row is derived from core issue data
-- and omitted from portable exports. The schema bootstrap applies this asset
-- only when pgvector 0.7 or later is available in public.
CREATE TABLE issue_vector_mirror (
  issue_uid        TEXT PRIMARY KEY,
  project_uid      TEXT NOT NULL,
  content          TEXT NOT NULL,
  content_revision BIGINT NOT NULL
);

CREATE TABLE issue_vector_generations (
  id BIGINT GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME issue_vector_generations_ordinal_seq) PRIMARY KEY,
  gen_key    TEXT NOT NULL UNIQUE,
  fingerprint TEXT NOT NULL,
  dimension INTEGER NOT NULL CONSTRAINT issue_vector_generations_dimension_check CHECK(dimension > 0),
  state      TEXT NOT NULL CHECK(state IN ('building','active','retired'))
);

CREATE TABLE issue_vector_stamps (
  generation_id BIGINT NOT NULL,
  doc_key TEXT NOT NULL,
  revision BIGINT NOT NULL,
  CONSTRAINT issue_vector_stamps_pkey PRIMARY KEY(generation_id, doc_key),
  CONSTRAINT issue_vector_stamps_generation_id_fkey FOREIGN KEY(generation_id) REFERENCES issue_vector_generations(id) ON DELETE CASCADE,
  CONSTRAINT issue_vector_stamps_doc_key_fkey FOREIGN KEY(doc_key) REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE
);

CREATE TABLE issue_vector_chunks (
  generation_id BIGINT NOT NULL,
  doc_key TEXT NOT NULL,
  chunk_index INTEGER NOT NULL,
  embedding  public.halfvec NOT NULL,
  CONSTRAINT issue_vector_chunks_pkey PRIMARY KEY(generation_id, doc_key, chunk_index),
  CONSTRAINT issue_vector_chunks_generation_id_fkey FOREIGN KEY(generation_id) REFERENCES issue_vector_generations(id) ON DELETE CASCADE,
  CONSTRAINT issue_vector_chunks_doc_key_fkey FOREIGN KEY(doc_key) REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE
);
CREATE INDEX idx_issue_vector_chunks_generation ON issue_vector_chunks(generation_id);
