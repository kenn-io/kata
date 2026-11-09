-- Named bindings retain their explicit account marker. The full owner and
-- generation stay in config_json; Go validates agreement before every use.
-- This real schema transition also prevents old binaries from ignoring named
-- identity and falling back to daemon-default provider credentials.
ALTER TABLE issue_sync_bindings ADD COLUMN connection_uid TEXT;
