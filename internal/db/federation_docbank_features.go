package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/metadata"
)

// DocbankOriginEventFeature identifies absolute Docbank reference origins.
const DocbankOriginEventFeature = "docbank_origin_v1"

func payloadHasDocbankOrigin(kind, payload string) (bool, error) {
	if kind != "projection.issue" && !strings.HasPrefix(kind, "issue.") {
		return false, nil
	}
	if payload == "" {
		return false, nil
	}
	var body struct {
		Metadata map[string]jsontext.Value `json:"metadata"`
		Diff     map[string]struct {
			From jsontext.Value `json:"from"`
			To   jsontext.Value `json:"to"`
		} `json:"diff"`
		Snapshot jsontext.Value `json:"snapshot"`
		Evidence []struct {
			Type       string         `json:"type"`
			Attachment jsontext.Value `json:"attachment"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return false, err
	}
	for key, value := range body.Metadata {
		if strings.HasPrefix(key, metadata.AttachmentPrefix) && docbankOriginReference(value) {
			return true, nil
		}
	}
	for key, value := range body.Diff {
		if strings.HasPrefix(key, metadata.AttachmentPrefix) && (docbankOriginReference(value.From) || docbankOriginReference(value.To)) {
			return true, nil
		}
	}
	for _, evidence := range body.Evidence {
		if evidence.Type == "attachment" && docbankOriginReference(evidence.Attachment) {
			return true, nil
		}
	}
	// Moves carry one issue projection; do not recursively classify arbitrary
	// user metadata or text as a transport document.
	if kind == "issue.moved" && len(body.Snapshot) > 0 {
		return payloadHasDocbankOrigin("projection.issue", string(body.Snapshot))
	}
	return false, nil
}

func docbankOriginReference(raw jsontext.Value) bool {
	var ref struct {
		Origin string `json:"origin"`
	}
	return json.Unmarshal(raw, &ref) == nil && ref.Origin != ""
}

func projectHasDocbankOrigin(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (bool, error) {
	// Probe only current candidate issue documents, never retained history on
	// every poll. Escaped keys remain candidates and are classified as JSON.
	rows, err := q.QueryContext(ctx, `SELECT i.metadata FROM issues i JOIN projects p ON p.id=i.project_id
WHERE p.uid=$1 AND (i.metadata LIKE '%"attachment.%' OR i.metadata LIKE '%\u%' ESCAPE '!')`, projectUID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		found, err := payloadHasDocbankOrigin("projection.issue", `{"metadata":`+raw+`}`)
		if err != nil || found {
			return found, err
		}
	}
	return false, rows.Err()
}

// RelayBatchRequiredFeatures inspects exact event source bytes without
// rewriting envelopes; receipts and embedding artifacts contain no issue data.
func RelayBatchRequiredFeatures(batch RelayBatch) (string, error) {
	if batch.Stream != RelayStreamEvent {
		return "", nil
	}
	var required string
	for _, envelope := range batch.Envelopes {
		event, err := DecodeRelaySourceEvent(envelope.Body)
		if err != nil {
			return "", err
		}
		origin, err := payloadHasDocbankOrigin(event.Type, string(event.Payload))
		if err != nil {
			return "", err
		}
		if origin {
			required = DocbankOriginEventFeature
		}
	}
	return required, nil
}

// RelayResetRequiredFeatures covers both original history and current-state
// source records, including a retained checkpoint prepared before this request.
func RelayResetRequiredFeatures(snapshot RootResetSnapshot) (string, error) {
	var required string
	for _, section := range [][]byte{snapshot.Events, snapshot.Entities} {
		if len(section) == 0 || string(section) == "null" {
			continue
		}
		var bodies [][]byte
		if err := json.Unmarshal(section, &bodies); err != nil {
			return "", err
		}
		for _, body := range bodies {
			event, err := DecodeRelaySourceEvent(body)
			if err != nil {
				return "", err
			}
			origin, err := payloadHasDocbankOrigin(event.Type, string(event.Payload))
			if err != nil {
				return "", err
			}
			if origin {
				required = DocbankOriginEventFeature
			}
		}
	}
	return required, nil
}
