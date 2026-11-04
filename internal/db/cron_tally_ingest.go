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
	if event.Type == "project.metadata_updated" {
		for key, raw := range PayloadMap(PayloadMap(event.Payload)["diff"]) {
			if !strings.HasPrefix(key, CronTallyPrefix) {
				continue
			}
			touched = true
			to := PayloadMap(raw)["to"]
			if string(to) != "null" {
				fields[key] = to
			}
		}
	} else if event.Type == "project.federation_enabled" {
		for key, raw := range PayloadMap(PayloadMap(event.Payload)["metadata"]) {
			if strings.HasPrefix(key, CronTallyPrefix) {
				fields[key] = raw
				touched = true
			}
		}
	}
	raw, err := json.Marshal(fields, json.Deterministic(true))
	return JSONBlob(raw), touched, err
}

// ValidateCronTallyDownstreamEvent checks fresh authenticated downstream
// ingress. Source-origin assertions never create authority; direct adoption
// is the bounded exception already verified by its enrollment continuation.
func ValidateCronTallyDownstreamEvent(event RemoteEvent, allowAdoption bool) error {
	_, touched, err := CronTallyEventMetadata(event)
	if err != nil {
		return err
	}
	if touched && !allowAdoption {
		return fmt.Errorf("%w: pruning tallies require root authority or a bounded adoption grant", ErrFederationIngestValidation)
	}
	return nil
}
