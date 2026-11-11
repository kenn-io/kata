package db

import "testing"

// Retained evidence is publishable only when the argv representation decodes;
// historical command and prompt records retain their existing decode rules.
func TestRetainedWorkflowArgvActivation(t *testing.T) {
	for _, tc := range []struct {
		name, step string
		argv, held bool
	}{
		{"valid", `{"key":"run","kind":"argv","argv":["printf","", "hello"]}`, true, false},
		{"missing", `{"key":"run","kind":"argv"}`, false, true},
		{"null", `{"key":"run","kind":"argv","argv":null}`, false, true},
		{"empty", `{"key":"run","kind":"argv","argv":[]}`, false, true},
		{"empty program", `{"key":"run","kind":"argv","argv":[""]}`, false, true},
		{"nonstring", `{"key":"run","kind":"argv","argv":["echo",null]}`, false, true},
		{"mixed", `{"key":"run","kind":"argv","argv":["echo"],"command":""}`, false, true},
		{"legacy", `{"key":"run","kind":"command","command":"echo"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"definition":{"version":1,"steps":[` + tc.step + `]}}`
			argv, held := retainedWorkflowEvidence("cron.workflow.snapshot", payload)
			if argv != tc.argv || held != tc.held {
				t.Fatalf("got argv=%t held=%t; want argv=%t held=%t", argv, held, tc.argv, tc.held)
			}
		})
	}
}

func TestResetEventFeaturesRequireRecipientSupport(t *testing.T) {
	valid := RemoteEvent{Type: "cron.workflow.snapshot", Payload: []byte(`{"definition":{"version":1,"steps":[{"key":"run","kind":"argv","argv":["echo",""]}]}}`)}
	legacy := RemoteEvent{Type: "cron.workflow.snapshot", Payload: []byte(`{"definition":{"version":1,"steps":[{"key":"run","kind":"command","command":"echo"}]}}`)}
	malformed := RemoteEvent{Type: "cron.workflow.snapshot", Payload: []byte(`{"definition":{"version":1,"steps":[{"key":"run","kind":"argv","argv":[]}]}}`)}
	for _, tc := range []struct {
		name     string
		event    RemoteEvent
		features string
		wantErr  bool
	}{
		{"legacy peer", legacy, "", false},
		{"old peer", valid, CronEventFeature, true},
		{"capable peer", valid, CronWorkflowArgvEventFeature, false},
		{"malformed even when capable", malformed, CronWorkflowArgvEventFeature, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireResetRecipientFeatures([]RemoteEvent{tc.event}, tc.features)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}
