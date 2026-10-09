package db

// AttributionUIResetMetadataPrefix identifies local stream invalidation cursors.
// Owner backups retain them; project exports never transport local UI state.
const AttributionUIResetMetadataPrefix = "attribution_ui_reset."
