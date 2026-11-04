package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/metadata"
)

// CronProjectPauseMetadataKey is root-owned cron admission policy.
const CronProjectPauseMetadataKey = "cron.pause"

// CronProjectPause describes a retained pause. Invalid is a fail-closed read
// marker for old malformed state, never a writable member of the value.
type CronProjectPause struct {
	PausedBy string    `json:"paused_by,omitempty"`
	PausedAt time.Time `json:"paused_at,omitzero"`
	Reason   string    `json:"reason"`
	Invalid  bool      `json:"invalid,omitempty"`
}

type cronPauseValue struct {
	PausedBy string    `json:"paused_by"`
	PausedAt time.Time `json:"paused_at"`
	Reason   string    `json:"reason"`
}

func decodeCronProjectPause(raw jsontext.Value) (CronProjectPause, error) {
	var fields map[string]jsontext.Value
	var value cronPauseValue
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 {
		return CronProjectPause{}, fmt.Errorf("%w: invalid cron pause fields", cron.ErrInvalid)
	}
	for _, key := range []string{"paused_by", "paused_at", "reason"} {
		if value, ok := fields[key]; !ok || value.Kind() != jsontext.KindString {
			return CronProjectPause{}, fmt.Errorf("%w: missing cron pause %s", cron.ErrInvalid, key)
		}
	}
	if err := json.Unmarshal(raw, &value, json.RejectUnknownMembers(true)); err != nil {
		return CronProjectPause{}, fmt.Errorf("%w: invalid cron pause: %v", cron.ErrInvalid, err)
	}
	if strings.TrimSpace(value.PausedBy) == "" || len(value.PausedBy) > 256 || value.PausedAt.IsZero() || len(value.Reason) > 256 {
		return CronProjectPause{}, fmt.Errorf("%w: invalid cron pause values", cron.ErrInvalid)
	}
	return CronProjectPause{PausedBy: value.PausedBy, PausedAt: value.PausedAt, Reason: value.Reason}, nil
}

// CronProjectPauseFromMetadata treats malformed present state as paused. Only
// absence or null means unpaused; other malformed values stay blocked.
func CronProjectPauseFromMetadata(blob JSONBlob) *CronProjectPause {
	if blob == "" {
		return nil
	}
	var values map[string]jsontext.Value
	if err := json.Unmarshal([]byte(blob), &values); err != nil {
		return &CronProjectPause{Invalid: true}
	}
	raw, exists := values[CronProjectPauseMetadataKey]
	if !exists || raw.Kind() == jsontext.KindNull {
		return nil
	}
	value, err := decodeCronProjectPause(raw)
	if err != nil {
		return &CronProjectPause{Invalid: true}
	}
	return &value
}

// cronControlKey identifies metadata reserved for root-owned cron controls.
func cronControlKey(key string) bool {
	return key == CronProjectPauseMetadataKey || strings.HasPrefix(key, CronRequestMetadataPrefix) || cronPassoverKey(key)
}

// CheckCronControlAdoption prevents retained root policy from becoming a
// spoke-origin baseline. Null controls are absent under metadata semantics.
func CheckCronControlAdoption(project Project) error {
	var values map[string]jsontext.Value
	if len(project.Metadata) == 0 || json.Unmarshal([]byte(project.Metadata), &values) != nil {
		return nil // Existing adoption validation reports corrupt metadata.
	}
	for key, value := range values {
		if !cronControlKey(key) || value.Kind() == jsontext.KindNull {
			continue
		}
		instruction := "a project with retained cron passover cannot be adopted into another hub"
		if key == CronProjectPauseMetadataKey {
			instruction = "resume the project cron pause before federation adoption"
		}
		if strings.HasPrefix(key, CronRequestMetadataPrefix) {
			instruction = "wait until retained requests expire and the root collects them before federation adoption"
		}
		return fmt.Errorf("%w: %s", ErrFederatedSpokeUnsupported, instruction)
	}
	return nil
}

// CheckCronProjectMetadataWrite reserves typed cron control state. Other
// cron-prefixed project metadata remains ordinary user metadata.
func CheckCronProjectMetadataWrite(values map[string]jsontext.Value) error {
	for key := range values {
		if cronControlKey(key) {
			return fmt.Errorf("%w: %s requires the typed cron control API", metadata.ErrInvalidValue, key)
		}
	}
	return nil
}

// ValidateCronControlEvent authenticates root policy independently of a peer's
// asserted origin. authorityUID is the pinned root; untrusted ingress uses "".
// Deletion accepts malformed old values so a root can recover legacy state.
func ValidateCronControlEvent(event RemoteEvent, authorityUID string) error {
	if event.Type != "project.metadata_updated" {
		return nil
	}
	var payload struct {
		Diff map[string]struct {
			From jsontext.Value `json:"from"`
			To   jsontext.Value `json:"to"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("%w: invalid cron control diff", ErrFederationIngestValidation)
	}
	for key, change := range payload.Diff {
		if !cronControlKey(key) {
			continue
		}
		if authorityUID == "" || event.OriginInstanceUID != authorityUID {
			return fmt.Errorf("%w: cron controls require root origin", ErrFederationIngestValidation)
		}
		if err := validateCronControlChange(key, change.From, change.To); err != nil {
			return err
		}
	}
	return nil
}

// validateCronControlChange checks a control value after root authentication.
func validateCronControlChange(key string, from, to jsontext.Value) error {
	var err error
	switch {
	case cronPassoverKey(key):
		err = ValidateCronPassoverChange(key, from, to)
	case strings.HasPrefix(key, CronRequestMetadataPrefix):
		err = validateCronRequestEventChange(strings.TrimPrefix(key, CronRequestMetadataPrefix), from, to)
	case key == CronProjectPauseMetadataKey && to.Kind() != jsontext.KindNull:
		_, err = decodeCronProjectPause(to)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	return nil
}

// SetCronProjectPauseIn changes only the project's cron admission switch.
type SetCronProjectPauseIn struct {
	ProjectID        int64
	Actor, Reason    string
	Paused           bool
	DiscardMissed    bool
	ExpectedRevision *int64
}
