
-- Root public-key pins and proofs are portable. Private signing material is not.
CREATE TABLE federation_root_keys (
 project_uid TEXT NOT NULL REFERENCES projects(uid) ON DELETE CASCADE,
 authority_uid TEXT NOT NULL,
 key_id TEXT NOT NULL,
 public_key TEXT NOT NULL,
 active SMALLINT NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
 PRIMARY KEY(project_uid,key_id)
);
CREATE UNIQUE INDEX uniq_federation_root_active ON federation_root_keys(project_uid) WHERE active=1;
CREATE TABLE federation_event_provenance (
 project_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 content_hash TEXT NOT NULL,
 reset_epoch BIGINT NOT NULL CHECK(reset_epoch>0),
 sequence BIGINT NOT NULL CHECK(sequence>0),
 key_id TEXT NOT NULL,
 receipt TEXT NOT NULL,
 PRIMARY KEY(project_uid,event_uid),
 UNIQUE(project_uid,reset_epoch,sequence),
 FOREIGN KEY(project_uid,key_id) REFERENCES federation_root_keys(project_uid,key_id) ON DELETE CASCADE
);
CREATE TABLE federation_entity_provenance (
 project_uid TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('issue','comment')),
 entity_uid TEXT NOT NULL,
 event_uid TEXT NOT NULL,
 PRIMARY KEY(project_uid,kind,entity_uid),
 FOREIGN KEY(project_uid,event_uid) REFERENCES federation_event_provenance(project_uid,event_uid) ON DELETE CASCADE
);
