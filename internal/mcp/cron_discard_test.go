package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Discard is opt-in only after a successful capability read, with one retained
// revision and one mutation even if the server returns an uncertain outcome.
// Ordinary resume reads nothing first and omits discard_missed for older hubs.
func TestCronMCPDiscardContract(t *testing.T) {
	for _, project := range []bool{false, true} {
		for _, tc := range []struct {
			mode, controls string
			write          int // 0 when no write may happen
			explicit       bool
		}{
			{"missing", `[]`, 0, false}, {"unknown", `["future_control"]`, 0, false}, {"unavailable", "", 0, false},
			{"supported", `["resume_discard_missed"]`, 200, false}, {"explicit", `["resume_discard_missed"]`, 200, true},
			{"conflict", `["resume_discard_missed"]`, 409, false}, {"uncertain", `["resume_discard_missed"]`, 502, false},
			{"ordinary", `["resume_discard_missed"]`, 200, false},
		} {
			if project && (tc.explicit || tc.mode == "ordinary") {
				continue
			}
			t.Run(fmt.Sprintf("project=%v/%s", project, tc.mode), func(t *testing.T) {
				id, err := uid.New()
				require.NoError(t, err)
				revision, err := uid.New()
				require.NoError(t, err)
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
							var body map[string]any
							assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
							if tc.mode == "ordinary" {
								assert.NotContains(t, body, "discard_missed")
							} else {
								assert.Equal(t, true, body["discard_missed"])
								assert.Equal(t, revision, body["expected_event_uid"])
							}
						}
						body := `{"changed":true,"job":{}}`
						if tc.write != http.StatusOK {
							body = `{"error":{"code":"revision_conflict","message":"changed"}}`
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.write)
						_, _ = fmt.Fprint(w, body)
					default:
						_, _ = fmt.Fprint(w, `{"projects":[{"id":7,"name":"spoke-project"}],"project":{"id":7,"name":"spoke-project"}}`)
					}
				}))
				defer proxy.Close()
				client, err := kataclient.NewWithHTTPClient(proxy.URL, proxy.Client())
				require.NoError(t, err)
				session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 7, ProjectName: "spoke-project", Actor: "operator", Version: "test"})
				tool, args := "cron_job_resume", map[string]any{"uid": id, "discard_missed": true}
				switch {
				case project:
					tool, args = "cron_project_resume", map[string]any{"discard_missed": true}
				case tc.explicit:
					args["expected_event_uid"] = revision
				case tc.mode == "ordinary":
					delete(args, "discard_missed")
				}
				result := cronCall(t, session, tool, args)

				wantCapabilities, wantShows := 1, 0
				if tc.mode == "ordinary" {
					wantCapabilities = 0
				} else if tc.write != 0 && !project && !tc.explicit {
					wantShows = 1
				}
				require.Equal(t, wantCapabilities, capabilities)
				require.Equal(t, wantShows, shows)
				require.Equal(t, min(tc.write, 1), writes)
				require.Equal(t, tc.write != http.StatusOK, result.IsError, "%s", mustJSON(t, result))
			})
		}
	}
}
