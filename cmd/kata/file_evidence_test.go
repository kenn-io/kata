package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFileEvidence(t *testing.T) {
	for _, kind := range []string{"file", "file-sha256"} {
		got, err := parseEvidenceFlags([]string{kind + ":notes:reports/result.md"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "file", string(got[0].Type))
		require.Equal(t, "notes", got[0].File.Source)
		require.Equal(t, "reports/result.md", got[0].File.Path)
		if kind == "file-sha256" {
			require.NotNil(t, got[0].File.CaptureSHA256)
			require.True(t, *got[0].File.CaptureSHA256)
		}
	}
	for _, v := range []string{"file:", "file:notes:", "file:notes:../x", "file:/x", "file:notes:/x"} {
		_, err := parseEvidenceFlags([]string{v})
		require.Error(t, err, v)
	}
}

func TestFileEvidenceDaemonCompatibility(t *testing.T) {
	for _, version := range []string{"0.49.0", "0.50.0"} {
		for _, operation := range []string{"read", "file", "file-sha256", "file-no-sha256", "file-with-docbank"} {
			if version == "0.50.0" && operation == "file-with-docbank" {
				continue // Attachment creation is covered by its existing integration tests.
			}
			t.Run(version+"/"+operation, func(t *testing.T) {
				var operations atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/projects/resolve":
						_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
					case "/api/v1/health":
						_, _ = fmt.Fprintf(w, `{"ok":true,"api_schema_version":%q}`, version)
					case "/api/v1/projects/1/issues/abc1/file-evidence":
						operations.Add(1)
						_, _ = w.Write([]byte(`{"event_uid":"event","index":0,"status":"missing","reference":{"source":"notes","path":"report.md"}}`))
					case "/api/v1/projects/1/issues/abc1/actions/close":
						operations.Add(1)
						_, _ = w.Write([]byte(`{"changed":true,"issue":{"id":1,"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","short_id":"abc1","status":"closed"}}`))
					default:
						operations.Add(1)
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				args := []string{"--project", "example-project", "--json"}
				if operation == "read" {
					args = append(args, "evidence", "abc1")
				} else {
					kind := operation
					if operation == "file-with-docbank" {
						kind = "file"
					}
					args = append(args, "close", "abc1", "--done", "--message", "Completed the requested work and retained its complete file proof.", "--evidence", kind+":notes:report.md")
					if operation == "file-with-docbank" {
						args = append(args, "--docbank", "/synthetic/report.md")
					}
				}
				_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), args...)
				if version == "0.49.0" {
					require.ErrorContains(t, err, "requires daemon API 0.50.0 or newer")
					require.Zero(t, operations.Load(), "reject before file read, close or attachment side effects")
				} else {
					require.NoError(t, err)
					require.Equal(t, int32(1), operations.Load())
				}
			})
		}
	}
}
