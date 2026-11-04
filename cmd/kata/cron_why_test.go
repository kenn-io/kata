package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronJobShowWhyAddsOnlyDispatch(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	body := cronCLIJob(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: body.Name, Actor: "operator", Definition: body.Definition.Native()})
	require.NoError(t, err)
	plain, _, err := executeCronCLI(t, env.URL, nil, "job", "show", job.UID)
	require.NoError(t, err)
	why, _, err := executeCronCLI(t, env.URL, nil, "job", "show", job.UID, "--why")
	require.NoError(t, err)
	var before, after map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(plain), &before))
	require.NoError(t, json.Unmarshal([]byte(why), &after))
	require.NotContains(t, before, "dispatch")
	var dispatch struct {
		Gate struct {
			Reason string `json:"reason"`
		} `json:"gate"`
	}
	require.NoError(t, json.Unmarshal(after["dispatch"], &dispatch))
	require.Equal(t, "external_adapter", dispatch.Gate.Reason)
	require.Contains(t, why, "9007199254740993")
	delete(after, "dispatch")
	require.Equal(t, before, after)
}

func TestCronJobShowWhyRequiresCurrentDaemon(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	body := cronCLIJob(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: body.Name, Actor: "operator", Definition: body.Definition.Native()})
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.40.0"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/dispatch") {
			calls++
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, _, err = executeCronCLI(t, server.URL, nil, "job", "show", job.UID, "--why")
	require.ErrorContains(t, err, "requires daemon API 0.41.0 or newer")
	require.Zero(t, calls)
}

// A malformed peer response must fail through the public CLI without a panic.
// A valid-object show response cannot exercise this parser contract.
func TestCronJobShowWhyRejectsNullShowResponse(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	body := cronCLIJob(t)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: body.Name, Actor: "operator", Definition: body.Definition.Native()})
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cron/jobs/"+job.UID) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("null"))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	_, _, err = executeCronCLI(t, server.URL, nil, "job", "show", job.UID, "--why")
	require.ErrorContains(t, err, "cron job show response must be a JSON object")
}
