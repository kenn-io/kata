package main

import (
	"context"
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
			_, err := closeEvidenceForShow(context.Background(), server.Client(), server.URL, 1, subject)
			require.ErrorContains(t, err, "issue history")
		})
	}
}

func TestShowAttachmentHistoryRejectsResetAndForeignClose(t *testing.T) {
	const subject = "01K7E222222222222222222222"
	for _, body := range []string{`{"events":[],"next_after_id":0,"reset_required":true}`, `{"events":[{"event_id":1,"type":"issue.closed","issue_uid":"01K7E333333333333333333333","payload":{"evidence":[]}}],"next_after_id":1,"reset_required":false}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Kata-Events-Issue-UID", subject)
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			_, err := closeEvidenceForShow(context.Background(), server.Client(), server.URL, 1, subject)
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
						events[i] = map[string]any{"event_id": i + 1, "type": "issue.updated"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "next_after_id": 1000, "reset_required": false})
				} else {
					require.Equal(t, "1000", r.URL.Query().Get("after_id"))
					if ack != "" {
						w.Header().Set("X-Kata-Events-Issue-UID", ack)
					}
					_, _ = fmt.Fprint(w, `{"events":[{"event_id":1001,"type":"issue.closed","issue_uid":"`+subject+`","payload":{"evidence":[{"type":"attachment","attachment_id":"report"}]}}],"next_after_id":1001,"reset_required":false}`)
				}
			}))
			defer server.Close()
			evidence, err := closeEvidenceForShow(context.Background(), server.Client(), server.URL, 1, subject)
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
