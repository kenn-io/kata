package tui

import (
	"encoding/json"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The hub operator must be able to rename and clear an enrolled instance
// through the same routes as the CLI, without changing another instance.
func TestFederationHubSpokeNames(t *testing.T) {
	const uid = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	for _, detail := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "detail"}[detail], func(t *testing.T) {
			env := testenv.New(t)
			for _, row := range []db.CreateFederationEnrollmentParams{
				{SpokeInstanceUID: uid, SpokeName: "laptop", Actor: "member", Capabilities: "pull"},
				{SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EB", SpokeName: "occupied", Actor: "member", Capabilities: "pull"},
			} {
				_, err := env.DB.CreateFederationEnrollment(t.Context(), row)
				require.NoError(t, err)
			}
			m := setupFederationView()
			m.api = NewClient(env.URL, env.HTTP)
			m, cmd := updateModel(m, keyRune('s'))
			require.NotNil(t, cmd, "s opens the hub's enrolled spokes")
			m, _ = updateModel(m, cmd())
			require.Contains(t, stripANSI(renderFederation(m)), "laptop")
			if detail {
				m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				require.Contains(t, stripANSI(renderFederation(m)), uid)
			}
			for _, next := range []string{"q2-build", "Invalid!", "occupied", ""} {
				m, _ = updateModel(m, keyRune('N'))
				m, _ = updateModel(m, tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
				m, _ = updateModel(m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
				m, _ = updateModel(m, tea.PasteMsg{Content: next})
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				require.NotNil(t, cmd)
				// Duplicate Enter cannot dispatch a second write.
				var duplicate tea.Cmd
				m, duplicate = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				require.Nil(t, duplicate)
				m, _ = updateModel(m, cmd())
				names, err := env.DB.ListFederationSpokeNames(t.Context())
				require.NoError(t, err)
				require.Equal(t, "occupied", names["01HZNQ7VFPK1XGD8R5MABCD4EB"])
				if next == "Invalid!" || next == "occupied" {
					require.Equal(t, "q2-build", names[uid])
					if next == "Invalid!" {
						require.Contains(t, stripANSI(renderFederation(m)), "must match")
					} else {
						require.Contains(t, stripANSI(renderFederation(m)), "already")
					}
					m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
				} else {
					require.Equal(t, next, names[uid])
					require.Contains(t, stripANSI(renderFederation(m)), uid)
					if next != "" {
						require.Contains(t, stripANSI(renderFederation(m)), next)
					} else {
						require.NotContains(t, stripANSI(renderFederation(m)), "q2-build")
					}
				}
			}
			m, _ = updateModel(m, keyRune('N'))
			m, _ = updateModel(m, tea.PasteMsg{Content: "cancelled"})
			m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Nil(t, cmd)
			names, err := env.DB.ListFederationSpokeNames(t.Context())
			require.NoError(t, err)
			require.Empty(t, names[uid])
		})
	}
}

func TestFederationEnrollmentPreviewSpokeName(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	var enrollmentName string
	hub := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/enrollments": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			enrollmentName, _ = body["spoke_name"].(string)
			respondJSON(t, w, FederationEnrollment{Token: enrollmentSecret(), Actor: "member"})
		},
		"/api/v1/projects/42/federation/enable": func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(t, w, federationMetadataBody("spoke-project"))
		},
		"/api/v1/projects/42/federation/metadata": func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(t, w, federationMetadataBody("spoke-project"))
		},
	})
	m.federation.draft.HubTarget = daemonTargetWithResolvedAuth("hub", hub.URL, "", true)
	m.federation.draft.AdoptExisting = false
	m, _ = updateModel(m, keyRune('N'))
	for _, r := range "q2-build" {
		m, _ = updateModel(m, keyRune(r))
	}
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Contains(t, stripANSI(renderFederation(m)), "q2-build")
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	m, _ = updateModel(m, cmd())
	require.Equal(t, "q2-build", enrollmentName)
	require.NoError(t, m.federation.op.err)
}
