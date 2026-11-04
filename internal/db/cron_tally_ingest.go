package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// CronTallyEventMetadata extracts only complete reserved values. It validates
// both diff sides even when the event is an accepted duplicate.
func CronTallyEventMetadata(event RemoteEvent) (JSONBlob, bool, error) {
	if err := ValidateCronTallyEvent(event); err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	fields := map[string]jsontext.Value{}
	touched := false
	for key, raw := range cronTallyEventFields(event) {
		if !strings.HasPrefix(key, CronTallyPrefix) {
			continue
		}
		touched = true
		if event.Type == "project.metadata_updated" {
			raw = PayloadMap(raw)["to"]
		}
		if string(raw) != "null" {
			fields[key] = raw
		}
	}
	raw, err := json.Marshal(fields, json.Deterministic(true))
	return JSONBlob(raw), touched, err
}

// cronTallyEventFields selects the carrier already validated at ingress.
func cronTallyEventFields(event RemoteEvent) map[string]jsontext.Value {
	field := "diff"
	if event.Type == "project.federation_enabled" {
		field = "metadata"
	} else if event.Type != "project.metadata_updated" {
		return nil
	}
	return PayloadMap(PayloadMap(event.Payload)[field])
}

// ValidateCronTallyDownstreamEvent checks authority after value validation and
// accepted duplicate detection. Every ingress validates values before checking
// retries; this check only needs the reserved keys, not another decoded tally.
func ValidateCronTallyDownstreamEvent(event RemoteEvent, allowAdoption bool) error {
	for key := range cronTallyEventFields(event) {
		if strings.HasPrefix(key, CronTallyPrefix) && !allowAdoption {
			return fmt.Errorf("%w: pruning tallies require root authority or a bounded adoption grant", ErrFederationIngestValidation)
		}
	}
	return nil
}
