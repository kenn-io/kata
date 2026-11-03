package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShowAttachmentHistoryRequiresSubjectAcknowledgement(t *testing.T) {
	const subject = "01K7E222222222222222222222"
	for _, ack := range []string{"", "01K7E333333333333333333333"} {
		t.Run(ack, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if ack != "" {
					w.Header().Set("X-Kata-Events-Issue-UID", ack)
				}
				_, _ = fmt.Fprint(w, `{"events":[],"next_after_id":0,"reset_required":false}`)
			}))
			defer server.Close()
			_, err := closeEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
			require.ErrorContains(t, err, "issue history")
		})
	}
}

func TestShowAttachmentHistoryRejectsResetAndForeignClose(t *testing.T) {
	const subject = "01K7E222222222222222222222"
	for _, body := range []string{`{"events":[],"next_after_id":0,"reset_required":true}`, `{"events":[{"event_id":1,"event_uid":"foreign-close","type":"issue.closed","issue_uid":"01K7E333333333333333333333","payload":{"evidence":[]}}],"next_after_id":1,"reset_required":false,"effective_close_event_uid":"foreign-close"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Kata-Events-Issue-UID", subject)
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			_, err := closeEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
			require.ErrorContains(t, err, "issue history")
		})
	}
}

func TestShowAttachmentHistoryRequiresAcknowledgementOnEveryPage(t *testing.T) {
	const subject = "01K7E222222222222222222222"
	for _, ack := range []string{"", "01K7E333333333333333333333", subject} {
		t.Run(ack, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				w.Header().Set("Content-Type", "application/json")
				if pages == 1 {
					w.Header().Set("X-Kata-Events-Issue-UID", subject)
					events := make([]map[string]any, 1000)
					for i := range events {
						events[i] = map[string]any{"event_id": i + 1, "type": "issue.updated", "issue_uid": subject, "payload": map[string]any{}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "next_after_id": 1000, "reset_required": false, "effective_close_event_uid": "close-current"})
				} else {
					require.Equal(t, "1000", r.URL.Query().Get("after_id"))
					if ack != "" {
						w.Header().Set("X-Kata-Events-Issue-UID", ack)
					}
					_, _ = fmt.Fprint(w, `{"events":[{"event_id":1001,"event_uid":"close-current","type":"issue.closed","issue_uid":"`+subject+`","payload":{"evidence":[{"type":"attachment","attachment_id":"report"}]}}],"next_after_id":1001,"reset_required":false}`)
				}
			}))
			defer server.Close()
			evidence, err := closeEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
			require.Equal(t, 2, pages)
			if ack != subject {
				require.ErrorContains(t, err, "issue history")
				return
			}
			require.NoError(t, err)
			require.Len(t, evidence, 1)
			require.Equal(t, "report", evidence[0].AttachmentID)
		})
	}
}

func TestOptionalShowAttachmentHistory(t *testing.T) {
	const subject = "01K7E222222222222222222222222"
	for _, tc := range []struct {
		name, version string
		status        int
		wantHistory   bool
	}{
		{"older daemon", "0.31.0", http.StatusOK, false},
		{"history API predates effective close selection", "0.39.0", http.StatusOK, false},
		{"history denied", "0.40.0", http.StatusForbidden, true},
		{"host hides history", "0.40.0", http.StatusNotFound, true},
		{"supported", "0.40.0", http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			historyCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = fmt.Fprintf(w, `{"api_schema_version":%q}`, tc.version)
					return
				}
				historyCalls++
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					return
				}
				w.Header().Set("X-Kata-Events-Issue-UID", subject)
				_, _ = fmt.Fprint(w, `{"events":[],"next_after_id":0,"reset_required":false}`)
			}))
			defer server.Close()
			_, err := optionalCloseEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
			require.NoError(t, err)
			require.Equal(t, tc.wantHistory, historyCalls > 0)
		})
	}
}

func TestShowAttachmentHistoryUsesServerSelectedCloseEvent(t *testing.T) {
	const subject = "01K7E222222222222222222222222"
	requested := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested <- r.URL.Query().Get("include_effective_close_event_uid") == "true"
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kata-Events-Issue-UID", subject)
		_, _ = fmt.Fprintf(w, `{"events":[{"event_id":1,"event_uid":"new-close","type":"issue.closed","issue_uid":%q,"payload":{"evidence":[{"type":"attachment","attachment_id":"new"}]}},{"event_id":2,"event_uid":"old-close","type":"issue.closed","issue_uid":%q,"payload":{"evidence":[{"type":"attachment","attachment_id":"old"}]}}],"next_after_id":2,"reset_required":false,"effective_close_event_uid":"new-close"}`, subject, subject)
	}))
	defer server.Close()

	evidence, err := closeEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
	require.NoError(t, err)
	require.True(t, <-requested, "history request should ask the daemon to select the effective close")
	require.Len(t, evidence, 1)
	require.Equal(t, "new", evidence[0].AttachmentID)
}

func TestShowAttachmentHistoryTracksProviderStatusTransitions(t *testing.T) {
	const subject = "01K7E222222222222222222222222"
	for _, tc := range []struct {
		name, updates string
		want          bool
	}{
		{"same state", `{"event_id":2,"event_uid":"update-1","type":"issue.updated","issue_uid":"` + subject + `","hlc_physical_ms":2,"payload":{"status":"closed"}}`, true},
		{"reopen and close", `{"event_id":2,"event_uid":"update-1","type":"issue.updated","issue_uid":"` + subject + `","hlc_physical_ms":2,"payload":{"status":"open"}},{"event_id":3,"event_uid":"update-2","type":"issue.updated","issue_uid":"` + subject + `","hlc_physical_ms":3,"payload":{"status":"closed"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Kata-Events-Issue-UID", subject)
				selected := ""
				if tc.want {
					selected = `,"effective_close_event_uid":"close-1"`
				}
				_, _ = fmt.Fprintf(w, `{"events":[{"event_id":1,"event_uid":"close-1","type":"issue.closed","issue_uid":%q,"hlc_physical_ms":1,"payload":{"evidence":[{"type":"attachment","attachment_id":"report"}]}},%s],"next_after_id":3,"reset_required":false%s}`, subject, tc.updates, selected)
			}))
			defer server.Close()
			evidence, err := closeEvidenceForShow(t.Context(), server.Client(), server.URL, 1, subject)
			require.NoError(t, err)
			require.Equal(t, tc.want, len(evidence) > 0)
		})
	}
}
