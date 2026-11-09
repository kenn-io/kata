package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestHistoricalMetadataJSONReferenceRoundTrip(t *testing.T) {
	for _, key := range []string{"", "example.key"} {
		for _, qualified := range []bool{false, true} {
			t.Run(fmt.Sprintf("key=%s/qualified=%t", key, qualified), func(t *testing.T) {
				env, dir, _, moved, oldRef := historicalOutputFixture(t)
				requestedRef := oldRef
				if qualified {
					requestedRef = "source-project#" + oldRef
				}
				runCLI(t, env, dir, "meta", "set", requestedRef, "example.key", "value")
				args := []string{"meta", "get", requestedRef, "--json"}
				if key != "" {
					args = append(args, key)
				}
				var result struct {
					Ref string `json:"ref"`
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, args...)), &result))
				var shown struct {
					Issue struct {
						UID string `json:"uid"`
					} `json:"issue"`
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "show", result.Ref, "--json")), &shown))
				require.Equal(t, moved.UID, shown.Issue.UID, "metadata JSON must retain a usable reference from the source workspace")
				require.Equal(t, requestedRef, result.Ref)
			})
		}
	}
}

func TestHistoricalSynthesizedJSONRetainsCurrentIdentity(t *testing.T) {
	for _, operation := range []string{"status", "scoped token"} {
		for _, displayUnavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/display-unavailable=%t", operation, displayUnavailable), func(t *testing.T) {
				var options []testenv.Option
				if operation == "scoped token" {
					options = []testenv.Option{testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity()}
				}
				env, dir, target, moved, oldRef := historicalOutputFixtureWithTarget(t, `target project "review"`, options...)
				if displayUnavailable {
					upstream, err := url.Parse(env.URL)
					require.NoError(t, err)
					forward := &httputil.ReverseProxy{
						Rewrite: func(request *httputil.ProxyRequest) { request.SetURL(upstream) },
					}
					proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == fmt.Sprintf("/api/v1/projects/%d", target.ID) {
							http.Error(w, "project display unavailable", http.StatusServiceUnavailable)
							return
						}
						forward.ServeHTTP(w, r)
					}))
					t.Cleanup(proxy.Close)
					env.URL = proxy.URL
				}
				args := []string{"status", oldRef, "--json"}
				if operation == "scoped token" {
					args = []string{"tokens", "create", "--issue", oldRef, "--actor", "worker", "--expires-in", "1h", "--token-file", privateTokenFilePath(t), "--json"}
				}
				var result struct {
					Issue   string      `json:"issue"`
					Project string      `json:"project"`
					Token   tokenCLIOut `json:"token"`
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, args...)), &result))
				expectedRef := target.Name + "#" + moved.ShortID
				if operation == "status" {
					expectedRef = moved.ShortID
				}
				if displayUnavailable {
					expectedRef = moved.UID
				}
				if operation == "scoped token" {
					require.NotNil(t, result.Token.Scope)
					require.Equal(t, target.UID, result.Token.Scope.ProjectUID)
					require.Equal(t, moved.UID, result.Token.Scope.RootIssueUID)
				} else if displayUnavailable {
					require.Empty(t, result.Project, "a failed display lookup must not name the former project")
				} else {
					require.Equal(t, target.Name, result.Project)
				}
				require.Equal(t, expectedRef, result.Issue)
				var shown struct {
					Issue struct {
						UID string `json:"uid"`
					} `json:"issue"`
				}
				roundtripRef := result.Issue
				if operation == "status" && result.Project != "" {
					roundtripRef = result.Project + "#" + result.Issue
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "show", roundtripRef, "--json")), &shown))
				require.Equal(t, moved.UID, shown.Issue.UID, "JSON reference must identify the moved issue from the source workspace")
			})
		}
	}
}
