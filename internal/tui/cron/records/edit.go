package records

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	native "go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/jsonutil"
)

// SameDefinition compares identity and native canonical documents, preserving exact option numbers.
func SameDefinition(a, b Job) bool {
	decode := func(raw []byte) ([]byte, error) {
		// The daemon serializes typed definitions, omitting optional zero values.
		// Canonicalize that same contract before comparing a lost-response readback.
		var canonical []byte
		var err error
		var tag struct {
			Kind string `json:"kind"`
		}
		if err = json.Unmarshal(raw, &tag); err != nil {
			return nil, err
		}
		if tag.Kind == "job" {
			var job native.JobDefinition
			job, err = native.ParseJob(raw)
			if err == nil {
				canonical, err = json.Marshal(job)
			}
		} else {
			var workflow native.WorkflowDefinition
			workflow, err = native.ParseWorkflow(raw)
			if err == nil {
				canonical, err = json.Marshal(workflow)
			}
		}
		if err != nil {
			// Incomplete form drafts are compared literally; only validated documents
			// can reach the write/readback path.
			canonical = raw
		}
		var v any
		d := jsontext.NewDecoder(bytes.NewReader(canonical))
		if err := json.UnmarshalDecode(d, &v, jsonutil.PreserveNumberLiterals()); err != nil {
			return nil, err
		}
		return json.Marshal(v, json.Deterministic(true))
	}
	x, xe := decode(a.Definition)
	y, ye := decode(b.Definition)
	return a.UID == b.UID && a.Name == b.Name && a.DeletedAt == nil && b.DeletedAt == nil && xe == nil && ye == nil && bytes.Equal(x, y)
}

// RetryClass classifies a native write outcome.
type RetryClass uint8

// Write outcome classes preserve the guarded retry decision.
const (
	Retry RetryClass = iota
	Conflict
	Fatal
)

// Status extracts the native HTTP status through wrapped errors.
func Status(err error) int {
	var e interface{ HTTPStatus() int }
	if errors.As(err, &e) {
		return e.HTTPStatus()
	}
	return 0
}

// ErrorCode retains structured authentication failures through wrapped errors.
func ErrorCode(err error) string {
	var e interface{ HTTPErrorCode() string }
	if errors.As(err, &e) {
		return e.HTTPErrorCode()
	}
	return ""
}

// AuthRejected distinguishes invalid credentials from project permission denials.
func AuthRejected(err error) bool {
	return Status(err) == 401 || ErrorCode(err) == "token_invalid" || ErrorCode(err) == "auth_invalid"
}

// Class distinguishes conflicts, uncertain writes, and definitive rejection.
func Class(err error) RetryClass {
	if Status(err) == 409 {
		return Conflict
	}
	if Ambiguous(err) {
		return Retry
	}
	return Fatal
}

// Ambiguous reports whether a failed write may have reached the daemon.
func Ambiguous(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, native.ErrInvalid) {
		return false
	}
	status := Status(err)
	return status == 0 || status >= 500 || status == 408
}

// ConflictError carries the daemon's exact current document and original guard.
type ConflictError struct {
	Pending, Newer  Job
	Remaining, Done []Job
	Operation       string
	Cause           error
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("guarded mutation needs review: %v", e.Cause)
}
func (e *ConflictError) Unwrap() error { return e.Cause }
