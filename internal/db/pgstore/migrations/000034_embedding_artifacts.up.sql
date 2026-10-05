
-- Portable original float32 artifacts are separate from the local vector index.
-- Non-null staging_expires_at never establishes durable relay acceptance.
CREATE TABLE federation_embedding_artifacts (
  project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
  digest TEXT NOT NULL CHECK(length(digest)=64),
  issue_uid TEXT NOT NULL CHECK(length(issue_uid)=26),
  input_hash TEXT NOT NULL CHECK(length(input_hash)=64),
  recipe_fingerprint TEXT NOT NULL CHECK(length(recipe_fingerprint)=64),
  vector_byte_size BIGINT NOT NULL CHECK(vector_byte_size BETWEEN 1 AND 16777216),
  manifest TEXT NOT NULL CHECK(length(manifest) BETWEEN 1 AND 2097152),
  artifact TEXT NOT NULL CHECK(length(artifact) BETWEEN 1 AND 33554432),
  staging_expires_at TEXT,
  PRIMARY KEY(project_uid,digest)
);
CREATE INDEX idx_embedding_artifacts_input ON federation_embedding_artifacts(project_uid,issue_uid,input_hash,recipe_fingerprint);
CREATE INDEX idx_embedding_artifacts_staging ON federation_embedding_artifacts(project_uid,staging_expires_at) WHERE staging_expires_at IS NOT NULL;
