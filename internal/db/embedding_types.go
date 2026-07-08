package db

// IssueEmbedding is one stored vector for an issue.
type IssueEmbedding struct {
	IssueID                 int64
	EmbeddedContentRevision int64
	Fingerprint             string
	Dims                    int
	Vector                  []float32 // L2-normalized
}

// EmbedTarget is an issue whose embedding is missing or stale for the active
// fingerprint, carrying the text the reconciler must embed.
type EmbedTarget struct {
	IssueID         int64
	ContentRevision int64
	Title           string
	Body            string
}

// IssueContent is one embeddable issue's text and identity for the vector
// mirror. ID is the pagination cursor; UID is the mirror/doc key.
type IssueContent struct {
	ID              int64
	UID             string
	ProjectUID      string
	Title           string
	Body            string
	ContentRevision int64
}
