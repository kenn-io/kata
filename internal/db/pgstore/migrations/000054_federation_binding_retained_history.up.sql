ALTER TABLE federation_bindings ADD COLUMN retained_through_event_id BIGINT NOT NULL DEFAULT 0;
