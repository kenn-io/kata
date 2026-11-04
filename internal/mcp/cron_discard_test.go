package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
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
func TestCronMCPDiscardContract(t *testing.T) {
	for _, projectAction := range []bool{false, true} {
		for _, mode := range []string{"missing", "unknown", "unavailable", "supported", "conflict", "uncertain", "explicit"} {
			t.Run(fmt.Sprintf("project=%v/%s", projectAction, mode), func(t *testing.T) {
				id, err := uid.New()
				require.NoError(t, err)
				revision, err := uid.New()
				require.NoError(t, err)
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
							require.Equal(t, "rev-7", r.Header.Get("If-Match"))
							require.Equal(t, "true", r.URL.Query().Get("discard_missed"))
						} else {
							var body struct {
								Discard  bool   `json:"discard_missed"`
								Expected string `json:"expected_event_uid"`
							}
							require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
							require.True(t, body.Discard)
							require.Equal(t, revision, body.Expected)
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
				client, err := kataclient.NewWithHTTPClient(proxy.URL, proxy.Client())
				require.NoError(t, err)
				session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 7, ProjectName: "spoke-project", Actor: "operator", Version: "test"})
				tool := "cron_job_resume"
				args := map[string]any{"uid": id, "discard_missed": true}
				if projectAction {
					tool = "cron_project_resume"
					delete(args, "uid")
				} else if mode == "explicit" {
					args["expected_event_uid"] = revision
				}
				result := cronCall(t, session, tool, args)

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
					require.False(t, result.IsError, "%s", mustJSON(t, result))
				} else {
					require.True(t, result.IsError)
				}
			})
		}
	}
}

func TestCronMCPOrdinaryResumeOldWire(t *testing.T) {
	client, err := kataclient.NewWithHTTPClient("http://daemon.example", &http.Client{Transport: cronOrdinaryTransport(t)})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 7, ProjectName: "spoke-project", Actor: "operator", Version: "test"})
	id, err := uid.New()
	require.NoError(t, err)
	result := cronCall(t, session, "cron_job_resume", map[string]any{"uid": id})
	require.False(t, result.IsError, "%s", mustJSON(t, result))
}

type cronRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cronRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cronOrdinaryTransport(t *testing.T) http.RoundTripper {
	return cronRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodPost, r.Method)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.NotContains(t, body, "discard_missed")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"job":{}}`)), Request: r}, nil
	})
}
