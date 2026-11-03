
CREATE TABLE cron_occurrence_claims (
 project_id BIGINT NOT NULL REFERENCES projects(id),
 job_uid TEXT NOT NULL,
 occurrence_key TEXT NOT NULL,
 run_uid TEXT NOT NULL UNIQUE,
 holder_instance_uid TEXT NOT NULL,
 actor TEXT NOT NULL,
 teammate TEXT,
 executor_label TEXT,
 definition_event_uid TEXT NOT NULL,
 workflow_uid TEXT,
 workflow_definition_event_uid TEXT,
 authority_instance_uid TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,job_uid,occurrence_key)
);
