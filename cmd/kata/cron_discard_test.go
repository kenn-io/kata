package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Discard is opt-in only after a successful capability read, with one retained
// revision and one mutation even if the server returns an uncertain outcome.
func TestCronCLIDiscardContract(t *testing.T) {
	for _, project := range []bool{false, true} {
		for _, tc := range []struct {
			mode, controls string
			write          int // 0 when no write may happen
			explicit       bool
		}{
			{"missing", `[]`, 0, false}, {"unknown", `["future_control"]`, 0, false}, {"unavailable", "", 0, false},
			{"supported", `["resume_discard_missed"]`, 200, false}, {"explicit", `["resume_discard_missed"]`, 200, true},
			{"conflict", `["resume_discard_missed"]`, 409, false}, {"uncertain", `["resume_discard_missed"]`, 502, false},
		} {
			if project && tc.explicit {
				continue
			}
			t.Run(fmt.Sprintf("project=%v/%s", project, tc.mode), func(t *testing.T) {
				id, revision := cronCLIUID(t), cronCLIUID(t)
				capabilities, shows, writes := 0, 0, 0
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/cron/capabilities"):
						capabilities++
						if tc.controls == "" {
							http.Error(w, "unavailable", http.StatusServiceUnavailable)
							return
						}
						_, _ = fmt.Fprintf(w, `{"controls":%s,"project_revision":7}`, tc.controls)
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/"+id):
						shows++
						_, _ = fmt.Fprintf(w, `{"job":{"uid":%q,"definition_event_uid":%q}}`, id, revision)
					case r.Method == http.MethodPost || r.Method == http.MethodDelete:
						writes++
						if project {
							assert.Equal(t, "rev-7", r.Header.Get("If-Match"))
							assert.Equal(t, "true", r.URL.Query().Get("discard_missed"))
						} else {
							var body struct {
								Discard  bool   `json:"discard_missed"`
								Expected string `json:"expected_event_uid"`
							}
							assert.NoError(t, json.UnmarshalRead(r.Body, &body))
							assert.True(t, body.Discard)
							assert.Equal(t, revision, body.Expected)
						}
						body := `{"changed":true}`
						if tc.write != http.StatusOK {
							body = `{"error":{"code":"revision_conflict","message":"changed"}}`
						}
						w.WriteHeader(tc.write)
						_, _ = fmt.Fprint(w, body)
					default:
						_, _ = fmt.Fprint(w, `{"projects":[{"id":7,"name":"spoke-project"}],"project":{"id":7,"name":"spoke-project"}}`)
					}
				}))
				defer proxy.Close()
				args := []string{"job", "resume", id, "--discard-missed"}
				if project {
					args = []string{"resume", "--discard-missed"}
				} else if tc.explicit {
					args = append(args, "--expected-event-uid", revision)
				}
				_, _, err := executeCronCLI(t, proxy.URL, nil, args...)
				require.Equal(t, 1, capabilities)
				wantShows := 0
				if tc.write != 0 && !project && !tc.explicit {
					wantShows = 1
				}
				require.Equal(t, wantShows, shows)
				require.Equal(t, min(tc.write, 1), writes)
				if tc.write == 200 {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
