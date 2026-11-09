package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// CronRequestMetadataPrefix reserves project metadata keys for runtime requests.
const (
	CronRequestMetadataPrefix = "cron.request."
	CronRequestKindRun        = "run"
	CronRequestKindCancel     = "cancel"
	CronRequestDefaultTTL     = 600
	CronRequestLimit          = 100
)

// ErrCronRequestExpired means a collected request UID cannot be admitted again.
var (
	ErrCronRequestExpired     = errors.New("cron request identity expired")
	ErrCronRequestConflict    = errors.New("cron request identity conflicts with retained request")
	ErrCronRequestUnavailable = errors.New("cron request target is unavailable")
	ErrCronRequestCapacity    = errors.New("project has 100 live cron requests")
)

// CronRequest is a root-admitted request and its locally derived answer state.
// UID lives in the metadata key; State is never part of the stored value.
type CronRequest struct {
	UID                string    `json:"uid"`
	Kind               string    `json:"kind"`
	JobUID             string    `json:"job_uid,omitempty"`
	DefinitionEventUID string    `json:"definition_event_uid,omitempty"`
	RunUID             *string   `json:"run_uid,omitempty"`
	RequestedBy        string    `json:"requested_by"`
	RequestedAt        time.Time `json:"requested_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	State              string    `json:"state,omitempty"`
}

type cronRequestValue struct {
	Kind               string    `json:"kind"`
	JobUID             string    `json:"job_uid,omitempty"`
	DefinitionEventUID string    `json:"definition_event_uid,omitempty"`
	RunUID             *string   `json:"run_uid,omitempty"`
	RequestedBy        string    `json:"requested_by"`
	RequestedAt        time.Time `json:"requested_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// RequestCronIn identifies a retry by kind, target, resolved actor and TTL.
// Now is supplied by the daemon clock; zero selects the current time.
type RequestCronIn struct {
	ProjectID                   int64
	UID, Kind, TargetUID, Actor string
	TTLSeconds                  int
	Now                         time.Time
}

// RequestCronOut retains the transaction event for immediate publication.
type RequestCronOut struct {
	Request  CronRequest `json:"request"`
	Replayed bool        `json:"replayed"`
	Event    *Event      `json:"event,omitempty"`
}

func decodeCronRequest(id string, raw jsontext.Value) (CronRequest, error) {
	var value cronRequestValue
	members, err := decodeCronControl(raw, &value)
	if err != nil || !cronUID(id) {
		return CronRequest{}, errInvalidCronRequest
	}
	// Explicit nulls are present members: a run names no run_uid, and a cancel
	// either omits both job members or names both.
	hasJob, hasDefinition := members["job_uid"] != nil, members["definition_event_uid"] != nil
	valid := hasJob == hasDefinition && (!hasJob || cronUID(value.JobUID) && cronUID(value.DefinitionEventUID))
	switch value.Kind {
	case CronRequestKindRun:
		valid = valid && hasJob && members["run_uid"] == nil
	case CronRequestKindCancel:
		valid = valid && value.RunUID != nil && cronUID(*value.RunUID)
	default:
		valid = false
	}
	requested := uid.MustTime(id)
	ttl := value.ExpiresAt.Sub(value.RequestedAt)
	if !valid || strings.TrimSpace(value.RequestedBy) == "" || len(value.RequestedBy) > 256 || requested.Year() > 9999 || !requested.Equal(value.RequestedAt) ||
		value.ExpiresAt.Year() > 9999 || ttl < 60*time.Second || ttl > 86400*time.Second || ttl%time.Second != 0 {
		return CronRequest{}, errInvalidCronRequest
	}
	return CronRequest{UID: id, Kind: value.Kind, JobUID: value.JobUID, DefinitionEventUID: value.DefinitionEventUID, RunUID: value.RunUID, RequestedBy: value.RequestedBy, RequestedAt: value.RequestedAt, ExpiresAt: value.ExpiresAt}, nil
}

var errInvalidCronRequest = fmt.Errorf("%w: invalid cron request value", cron.ErrInvalid)

func encodeCronRequest(request CronRequest) (jsontext.Value, error) {
	return json.Marshal(cronRequestValue{Kind: request.Kind, JobUID: request.JobUID, DefinitionEventUID: request.DefinitionEventUID, RunUID: request.RunUID, RequestedBy: request.RequestedBy, RequestedAt: request.RequestedAt, ExpiresAt: request.ExpiresAt})
}

func validateCronRequestFreshness(id string, now time.Time) error {
	requested := uid.MustTime(id)
	if requested.Year() > 9999 || requested.After(now.Add(5*time.Second)) {
		return fmt.Errorf("%w: request UID is ahead of the daemon clock; check clocks and submit a fresh UID", cron.ErrInvalid)
	}
	if requested.Before(now.Add(-30 * time.Second)) {
		return fmt.Errorf("%w: use a fresh request UID after checking clocks", ErrCronRequestExpired)
	}
	return nil
}

func validateCronRequestEventChange(id string, from, to jsontext.Value) error {
	if to.Kind() == jsontext.KindNull {
		return nil
	}
	next, err := decodeCronRequest(id, to)
	if err != nil {
		return err
	}
	if len(from) == 0 || from.Kind() == jsontext.KindNull {
		return nil
	}
	previous, err := decodeCronRequest(id, from)
	if err != nil {
		return err
	}
	if !sameCronRequestValue(previous, next) {
		return ErrCronRequestConflict
	}
	return nil
}

func sameCronRequestValue(a, b CronRequest) bool {
	return a.Kind == b.Kind && a.JobUID == b.JobUID && a.DefinitionEventUID == b.DefinitionEventUID && (a.RunUID == nil) == (b.RunUID == nil) && (a.RunUID == nil || *a.RunUID == *b.RunUID) &&
		a.RequestedBy == b.RequestedBy && a.RequestedAt.Equal(b.RequestedAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}

// CronRequestList filters retained values using one consistent evidence read.
// Now is the server read time; zero uses the current time.
type CronRequestList struct {
	ProjectID int64
	State     string
	Now       time.Time
}

func cronRequestReadState(answered bool, expiresAt, now time.Time) string {
	if answered {
		return "answered"
	}
	if !now.Before(expiresAt) {
		return "expired"
	}
	return "pending"
}
