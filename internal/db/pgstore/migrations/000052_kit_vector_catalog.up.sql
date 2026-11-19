DO $$
BEGIN
  IF to_regclass('issue_vector_generations') IS NOT NULL THEN
    ALTER TABLE issue_vector_generations RENAME COLUMN ordinal TO id;
    ALTER TABLE issue_vector_generations RENAME COLUMN model TO fingerprint;
    UPDATE issue_vector_generations SET fingerprint = gen_key;
    ALTER TABLE issue_vector_generations RENAME COLUMN dimensions TO dimension;
    ALTER TABLE issue_vector_generations RENAME CONSTRAINT issue_vector_generations_dimensions_check TO issue_vector_generations_dimension_check;

    ALTER TABLE issue_vector_stamps ADD COLUMN generation_id BIGINT;
    UPDATE issue_vector_stamps s SET generation_id = g.id FROM issue_vector_generations g WHERE s.gen_key = g.gen_key;
    ALTER TABLE issue_vector_stamps ALTER COLUMN generation_id SET NOT NULL;
    ALTER TABLE issue_vector_stamps DROP COLUMN gen_key;
    ALTER TABLE issue_vector_stamps DROP CONSTRAINT issue_vector_stamps_issue_uid_fkey;
    ALTER TABLE issue_vector_stamps RENAME COLUMN issue_uid TO doc_key;
    ALTER TABLE issue_vector_stamps ADD CONSTRAINT issue_vector_stamps_pkey PRIMARY KEY (generation_id, doc_key);
    ALTER TABLE issue_vector_stamps ADD CONSTRAINT issue_vector_stamps_generation_id_fkey FOREIGN KEY (generation_id) REFERENCES issue_vector_generations(id) ON DELETE CASCADE;
    ALTER TABLE issue_vector_stamps ADD CONSTRAINT issue_vector_stamps_doc_key_fkey FOREIGN KEY (doc_key) REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE;

    ALTER TABLE issue_vector_chunks ADD COLUMN generation_id BIGINT;
    UPDATE issue_vector_chunks c SET generation_id = g.id FROM issue_vector_generations g WHERE c.gen_key = g.gen_key;
    ALTER TABLE issue_vector_chunks ALTER COLUMN generation_id SET NOT NULL;
    ALTER TABLE issue_vector_chunks DROP COLUMN gen_key;
    ALTER TABLE issue_vector_chunks DROP CONSTRAINT issue_vector_chunks_issue_uid_fkey;
    ALTER TABLE issue_vector_chunks RENAME COLUMN issue_uid TO doc_key;
    ALTER TABLE issue_vector_chunks ADD CONSTRAINT issue_vector_chunks_pkey PRIMARY KEY (generation_id, doc_key, chunk_index);
    ALTER TABLE issue_vector_chunks ADD CONSTRAINT issue_vector_chunks_generation_id_fkey FOREIGN KEY (generation_id) REFERENCES issue_vector_generations(id) ON DELETE CASCADE;
    ALTER TABLE issue_vector_chunks ADD CONSTRAINT issue_vector_chunks_doc_key_fkey FOREIGN KEY (doc_key) REFERENCES issue_vector_mirror(issue_uid) ON DELETE CASCADE;
    CREATE INDEX idx_issue_vector_chunks_generation ON issue_vector_chunks(generation_id);
    ALTER TABLE issue_vector_mirror DROP COLUMN embed_gen;
  END IF;
END $$;
