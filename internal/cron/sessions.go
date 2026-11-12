package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/kata/internal/transcript"
)

// Session identifies one agent session used by a run.
type Session struct {
	Agent     string  `json:"agent"`
	SessionID string  `json:"session_id"`
	Step      *string `json:"step,omitzero" nullable:"false"`
}

// UnmarshalJSON distinguishes an omitted step from an explicitly null step.
func (s *Session) UnmarshalJSON(input []byte) error {
	type wireSession Session
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(input, &fields); err != nil {
		return err
	}
	if raw, ok := fields["step"]; ok && string(raw) == "null" {
		return invalid("invalid session step")
	}
	var value wireSession
	if err := json.Unmarshal(input, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*s = Session(value)
	return nil
}

// UnmarshalJSON preserves field presence so explicit null session values are rejected.
func (s *Summary) UnmarshalJSON(input []byte) error {
	type wireSummary Summary
	var value wireSummary
	if err := json.Unmarshal(input, &value, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*s = Summary(value)
	return s.validateSessions(input)
}

// HasSessions reports whether the summary uses the session feature.
func (s Summary) HasSessions() bool { return s.Sessions != nil || s.SessionsIncomplete != nil }

func (s Summary) validateSessions(input []byte) error {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(input, &fields); err != nil {
		return err
	}
	if raw, ok := fields["sessions"]; ok && (string(raw) == "null" || s.Sessions == nil || len(*s.Sessions) == 0 || len(*s.Sessions) > 64) {
		return invalid("sessions must contain 1 to 64 entries")
	}
	if raw, ok := fields["sessions_incomplete"]; ok && string(raw) != "true" {
		return invalid("sessions_incomplete must be true")
	}
	if s.Sessions == nil {
		return nil
	}
	seen := make(map[[2]string]bool, len(*s.Sessions))
	for _, session := range *s.Sessions {
		if err := (transcript.Transcript{Agent: session.Agent, SessionID: session.SessionID}).Validate(); err != nil {
			return invalid(err.Error())
		}
		if session.Step != nil && !portableKey(*session.Step) {
			return invalid("invalid session step")
		}
		key := [2]string{session.Agent, session.SessionID}
		if seen[key] {
			return invalid("duplicate session")
		}
		seen[key] = true
	}
	return nil
}
