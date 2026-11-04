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
	for _, projectAction := range []bool{false, true} {
		for _, mode := range []string{"missing", "unknown", "unavailable", "supported", "conflict", "uncertain", "explicit"} {
			if projectAction && mode == "explicit" {
				continue
			}
			t.Run(fmt.Sprintf("project=%v/%s", projectAction, mode), func(t *testing.T) {
				id, revision := cronCLIUID(t), cronCLIUID(t)
				capabilities, shows, writes := 0, 0, 0
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/cron/capabilities"):
						capabilities++
						if mode == "unavailable" {
							http.Error(w, "unavailable", http.StatusServiceUnavailable)
							return
						}
						controls := `[]`
						if mode == "unknown" {
							controls = `["future_control"]`
						} else if mode != "missing" {
							controls = `["resume_discard_missed"]`
						}
						_, _ = fmt.Fprintf(w, `{"controls":%s,"project_revision":7}`, controls)
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/"+id):
						shows++
						_, _ = fmt.Fprintf(w, `{"job":{"uid":%q,"definition_event_uid":%q}}`, id, revision)
					case r.Method == http.MethodPost || r.Method == http.MethodDelete:
						writes++
						if projectAction {
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
						if mode == "conflict" {
							http.Error(w, `{"error":{"code":"revision_conflict","message":"changed"}}`, http.StatusConflict)
							return
						}
						if mode == "uncertain" {
							http.Error(w, "uncertain", http.StatusBadGateway)
							return
						}
						_, _ = fmt.Fprint(w, `{"changed":true}`)
					default:
						_, _ = fmt.Fprint(w, `{"projects":[{"id":7,"name":"spoke-project"}],"project":{"id":7,"name":"spoke-project"}}`)
					}
				}))
				defer proxy.Close()
				args := []string{"job", "resume", id, "--discard-missed"}
				if projectAction {
					args = []string{"resume", "--discard-missed"}
				} else if mode == "explicit" {
					args = append(args, "--expected-event-uid", revision)
				}
				_, _, err := executeCronCLI(t, proxy.URL, nil, args...)
				require.Equal(t, 1, capabilities)
				supported := mode == "supported" || mode == "conflict" || mode == "uncertain" || mode == "explicit"
				if supported {
					require.Equal(t, 1, writes)
					if !projectAction && mode != "explicit" {
						require.Equal(t, 1, shows)
					} else {
						require.Zero(t, shows)
					}
				} else {
					require.Zero(t, writes)
					require.Zero(t, shows)
				}
				if mode == "supported" || mode == "explicit" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}
