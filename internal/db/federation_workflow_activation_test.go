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
	snapshot := func(definition string) RemoteEvent {
		return RemoteEvent{
			EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAD", OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAC",
			ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Actor: "worker", HLCPhysicalMS: 2,
			Type: "cron.workflow.snapshot",
			Payload: []byte(`{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB",` +
				`"name":"Review","author":"worker","created_at":"2026-10-03T12:00:00Z","updated_at":"2026-10-03T12:00:01Z",` +
				`"definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAE",` +
				`"definition_hlc":{"version":1,"physical_ms":1,"counter":0,"origin_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAC"},` +
				`"definition":` + definition + `}`),
		}
	}
	valid := snapshot(`{"version":1,"steps":[{"key":"run","kind":"argv","argv":["echo",""]}]}`)
	legacy := snapshot(`{"version":1,"steps":[{"key":"run","kind":"command","command":"echo"}]}`)
	malformed := snapshot(`{"version":1,"steps":[{"key":"run","kind":"argv","argv":[]}]}`)
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
			events := []RemoteEvent{tc.event}
			err := requireUsableResetEvents(events)
			if err == nil {
				ctx := WithRelayPeerFeatures(t.Context(), "recipient", tc.features)
				err = requireResetRecipientFeatures(ctx, "recipient", events)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}
