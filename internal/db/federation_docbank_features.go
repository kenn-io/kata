package db

import (
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
	var body map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return false, err
	}
	return docbankOriginFromBody(kind, body)
}

func docbankOriginFromBody(kind string, body map[string]jsontext.Value) (bool, error) {
	if kind != "projection.issue" && !strings.HasPrefix(kind, "issue.") {
		return false, nil
	}
	var fields struct {
		Metadata map[string]jsontext.Value `json:"metadata"`
		Diff     map[string]struct {
			From jsontext.Value `json:"from"`
			To   jsontext.Value `json:"to"`
		} `json:"diff"`
		Evidence []struct {
			Type       string         `json:"type"`
			Attachment jsontext.Value `json:"attachment"`
		} `json:"evidence"`
	}
	for key, target := range map[string]any{"metadata": &fields.Metadata, "diff": &fields.Diff, "evidence": &fields.Evidence} {
		if raw := body[key]; len(raw) > 0 {
			if err := json.Unmarshal(raw, target); err != nil {
				return false, err
			}
		}
	}
	for key, value := range fields.Metadata {
		if strings.HasPrefix(key, metadata.AttachmentPrefix) && docbankOriginReference(value) {
			return true, nil
		}
	}
	for key, value := range fields.Diff {
		if strings.HasPrefix(key, metadata.AttachmentPrefix) && (docbankOriginReference(value.From) || docbankOriginReference(value.To)) {
			return true, nil
		}
	}
	for _, evidence := range fields.Evidence {
		if evidence.Type == "attachment" && docbankOriginReference(evidence.Attachment) {
			return true, nil
		}
	}
	// Moves carry one issue projection; do not recursively classify arbitrary
	// user metadata or text as a transport document.
	if kind == "issue.moved" && len(body["snapshot"]) > 0 {
		return payloadHasDocbankOrigin("projection.issue", string(body["snapshot"]))
	}
	return false, nil
}

func docbankOriginReference(raw jsontext.Value) bool {
	var ref struct {
		Origin string `json:"origin"`
	}
	return json.Unmarshal(raw, &ref) == nil && ref.Origin != ""
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
