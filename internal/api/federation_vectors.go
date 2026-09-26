package api

// Federation vector replication statuses for one requested document.
const (
	// FederationVectorStatusOK carries the document's chunk vectors.
	FederationVectorStatusOK = "ok"
	// FederationVectorStatusSkipped means the hub stamped the document
	// without vectors (its provider rejected the content, or it is blank).
	FederationVectorStatusSkipped = "skipped"
	// FederationVectorStatusNotReady means the hub has no vectors for this
	// exact content yet. The hub never embeds on request.
	FederationVectorStatusNotReady = "not_ready"
	// FederationVectorStatusDeferred means the response hit its size budget
	// before this document; ask again without backing off.
	FederationVectorStatusDeferred = "deferred"
)

// FederationVectorGeneration describes the embedding generation a hub serves
// vectors from. Fingerprint is kit Generation.Fingerprint() over Model, Dims,
// and Params; a spoke imports only when it equals its own fingerprint.
type FederationVectorGeneration struct {
	Fingerprint string            `json:"fingerprint"`
	Model       string            `json:"model"`
	Dims        int               `json:"dims"`
	Params      map[string]string `json:"params"`
	// State is the hub's lifecycle state for this generation: "pending"
	// (not registered yet), "building", or "active".
	State string `json:"state"`
}

// FederationVectorLookupRequest is the enrollment-authenticated vector lookup
// transport route. It is authorized with the pull capability.
type FederationVectorLookupRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
	Body          FederationVectorLookupRequestBody
}

// FederationVectorLookupRequestBody names the documents a spoke wants, keyed
// by issue UID and the SHA-256 of the spoke's local recipe text.
type FederationVectorLookupRequestBody struct {
	// Fingerprint is the spoke's generation fingerprint. The hub returns
	// records only when it equals the hub's own; otherwise the response
	// carries just the hub's generation so the spoke can report a mismatch.
	Fingerprint string                      `json:"fingerprint"`
	Docs        []FederationVectorLookupDoc `json:"docs,omitempty"`
}

// FederationVectorLookupDoc is one requested document.
type FederationVectorLookupDoc struct {
	IssueUID      string `json:"issue_uid"`
	ContentSHA256 string `json:"content_sha256"`
}

// FederationVectorLookupResponse wraps FederationVectorLookupBody.
type FederationVectorLookupResponse struct {
	Body FederationVectorLookupBody
}

// FederationVectorLookupBody answers a lookup. Generation is absent when the
// hub has no embeddings configured. Records follow request order.
type FederationVectorLookupBody struct {
	Generation *FederationVectorGeneration `json:"generation,omitempty"`
	Records    []FederationVectorRecord    `json:"records"`
}

// FederationVectorRecord is one document's lookup result.
type FederationVectorRecord struct {
	IssueUID string `json:"issue_uid"`
	// ContentSHA256 is the hash of the text the hub's vectors describe. It
	// equals the requested hash for ok and skipped records.
	ContentSHA256 string                  `json:"content_sha256,omitempty"`
	Status        string                  `json:"status"`
	Chunks        []FederationVectorChunk `json:"chunks,omitempty"`
}

// FederationVectorChunk is one chunk vector: Dims little-endian float32 values,
// base64-encoded in JSON.
type FederationVectorChunk struct {
	Index  int    `json:"index"`
	Vector []byte `json:"vector"`
}
