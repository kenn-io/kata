package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

const receiptJobUID = "01J00000000000000000000001"
const receiptRevision = "01J00000000000000000000002"

func receiptFixture(t testing.TB, enabled, native bool) []byte {
	t.Helper()
	action := `{"kind":"execute","prompt":"Inspect the workspace"}`
	issue := `{"kind":"per-run","title":"Inspection"}`
	overlap, options := "forbid", `{"large":9007199254740993,"decimal":1.2300,"exponent":9.001e100}`
	if native {
		action = `{"kind":"notify","recipient":"worker"}`
		issue = `{"kind":"per-run","title":"Inspection","parent_uid":"01J00000000000000000000003"}`
		overlap, options = "allow", `{}`
	}
	raw := fmt.Appendf(nil, `{"job":{"uid":%q,"definition_event_uid":%q,"name":"Inspection","definition":{"version":1,"kind":"job","enabled":%t,"trigger":{"kind":"cron","cron":"0 * * * *","timezone":"UTC"},"action":%s,"issue":%s,"overlap":%q,"catchup":"all","options":%s}},"extra":{"retained":true,"nullable":null,"nested":[{},[]]}}`, receiptJobUID, receiptRevision, enabled, action, issue, overlap, options)
	var fixture struct {
		Job struct {
			Definition jsontext.Value `json:"definition"`
		} `json:"job"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	_, err := cron.ParseJob(fixture.Job.Definition)
	require.NoError(t, err, "a fixture must reach its diagnostic branch")
	return raw
}

type receiptTransport func(*http.Request) (*http.Response, error)

func (f receiptTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func receiptDaemon(t *testing.T, handler func(*http.Request) (int, string)) daemonAPI {
	t.Helper()
	return daemonAPI{ctx: t.Context(), baseURL: "http://daemon.example", client: &http.Client{Transport: receiptTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "daemon.example", r.URL.Host)
		require.True(t, strings.HasPrefix(r.URL.Path, "/api/v1/projects/7/cron/"))
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
		status, body := handler(r)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}}
}

func receiptPreview(r *http.Request, delivery, basis string) string {
	from := r.URL.Query().Get("from")
	at, _ := time.Parse(time.RFC3339Nano, from)
	at = at.Add(time.Hour)
	return fmt.Sprintf(`{"valid":true,"errors":[],"warnings":[],"schedule":{"delivery":%q,"basis":%q,"zone":"UTC","from":%q,"next":[{"at":%q,"local":%q}],"additive":true}}`, delivery, basis, from, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
}

// Product contract: opt-in only, one mutation response, advisory failure cannot
// turn a successful write into a retry signal. No daemon or real data is used.
func TestCronReceiptCommandOptInAndConflict(t *testing.T) {
	for _, action := range []string{"create", "update", "restore"} {
		for _, optedIn := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/receipt=%t", action, optedIn), func(t *testing.T) {
				writes, reads := 0, 0
				raw := receiptFixture(t, true, false)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.URL.Path == "/api/v1/projects":
						_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
					case r.URL.Path == "/api/v1/projects/7":
						_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
					case strings.HasSuffix(r.URL.Path, "/validate") || strings.HasSuffix(r.URL.Path, "/runtimes"):
						reads++
						w.WriteHeader(http.StatusNotFound)
					case r.Method == http.MethodPut || strings.HasSuffix(r.URL.Path, "/restore"):
						writes++
						_, _ = w.Write(raw)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					}
				}))
				defer server.Close()
				args := []string{"job", action}
				if action == "create" {
					args = append(args, "--uid", receiptJobUID)
				} else {
					args = append(args, receiptJobUID, "--expected-event-uid", receiptRevision)
				}
				if action != "restore" {
					args = append(args, "--file", cronCLIFile(t, cronCLIJob(t)))
				}
				if optedIn {
					args = append(args, "--receipt")
				}
				out, _, err := executeCronCLI(t, server.URL, nil, args...)
				require.NoError(t, err)
				require.Equal(t, 1, writes)
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(out), &fields))
				if optedIn {
					require.Equal(t, 2, reads)
					assertReceiptPersisted(t, raw, fields)
					require.Equal(t, "1", string(fields["receipt_version"]))
				} else {
					require.Zero(t, reads)
					require.NotContains(t, out, "receipt_version")
					require.NotContains(t, out, "diagnostics")
					require.JSONEq(t, string(raw), string(fieldsValueWithoutStamp(t, fields)))
					require.Contains(t, out, "9007199254740993")
				}
			})
		}
	}
	t.Run("conflict", func(t *testing.T) {
		reads := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/projects":
				_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
			case "/api/v1/projects/7":
				_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
			default:
				if r.Method != http.MethodPut {
					reads++
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"cron_conflict","error":"revision changed"}`)
			}
		}))
		defer server.Close()
		out, _, err := executeCronCLI(t, server.URL, nil, "job", "update", receiptJobUID, "--file", cronCLIFile(t, cronCLIJob(t)), "--expected-event-uid", receiptRevision, "--receipt")
		require.Error(t, err)
		require.Empty(t, out)
		require.Zero(t, reads)
	})
}

func assertReceiptPersisted(t *testing.T, raw []byte, fields map[string]jsontext.Value) {
	t.Helper()
	require.Equal(t, "1", string(fields["kata_api_version"]))
	var got, want map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(fields["persisted"], &got))
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Equal(t, want, got, "raw nested members and number tokens must survive emitJSON")
	require.NotContains(t, string(fields["persisted"]), `"events"`, "absent events must remain absent")
	for _, token := range []string{"9007199254740993", "1.2300", "9.001e100"} {
		require.Contains(t, string(fields["persisted"]), token)
	}
}

func fieldsValueWithoutStamp(t *testing.T, fields map[string]jsontext.Value) []byte {
	t.Helper()
	delete(fields, "kata_api_version")
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

func TestCronReceiptAvailabilityFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"404", "private diagnostic body", "unsupported", 404},
		{"405", "", "unsupported", 405},
		{"501", "", "unsupported", 501},
		{"forbidden", "private diagnostic body", "unavailable", 403},
		{"server", "private diagnostic body", "unavailable", 500},
		{"invalid", "not-json", "unavailable", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := receiptDaemon(t, func(*http.Request) (int, string) { return tc.status, tc.body })
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, false), false)
			require.Equal(t, tc.want, got.Diagnostics.Preview.Status)
			require.Equal(t, tc.want, got.Diagnostics.RuntimeInventory.Status)
			raw, err := json.Marshal(got)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private diagnostic body")
			require.Contains(t, string(got.Persisted), receiptRevision)
		})
	}
}

// Both diagnostic responses accept at most 1 MiB, even when the JSON is valid.
func TestCronReceiptDiagnosticResponseSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		size int
		want string
	}{{1 << 20, "available"}, {(1 << 20) + 1, "unavailable"}} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				body := `{"inventory":{"state":"ready"},"warnings":[]}`
				if r.Method == http.MethodPost {
					body = receiptPreview(r, "adapter", "adapter_trigger")
				}
				return 200, body + strings.Repeat(" ", tc.size-len(body))
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, false), false)
			require.Equal(t, tc.want, got.Diagnostics.Preview.Status)
			require.Equal(t, tc.want, got.Diagnostics.RuntimeInventory.Status)
		})
	}
}

func TestCronReceiptQuietPausedDeletedAndNativeInventory(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		enabled, native, quiet bool
	}{
		{"quiet", true, false, true},
		{"paused", false, false, false},
		{"native", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				reads++
				require.True(t, strings.HasSuffix(r.URL.Path, "/validate"))
				return 200, receiptPreview(r, "daemon", "stored_progress")
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, tc.enabled, tc.native), tc.quiet)
			if tc.native {
				require.Equal(t, 1, reads)
				require.Equal(t, "not_applicable", got.Diagnostics.RuntimeInventory.Status)
			} else {
				require.Zero(t, reads)
				require.Equal(t, "skipped", got.Diagnostics.Preview.Status)
			}
		})
	}
}

func TestCronReceiptNativePreviewNeverInventsSavedProgress(t *testing.T) {
	for _, state := range []string{"stored_progress", "fresh_anchor", "parent_closed", "paused", "ownership", "stale_replica", "missing_next", "wrong_from", "bad_local", "unknown_delivery"} {
		t.Run(state, func(t *testing.T) {
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "3", r.URL.Query().Get("count"))
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Contains(t, string(body), receiptRevision)
				if state == "ownership" || state == "stale_replica" {
					return 200, `{"valid":false,"errors":[{"stage":"ownership","code":"validation","message":"checkpoint unavailable"}],"warnings":[]}`
				}
				response := receiptPreview(r, "daemon", "stored_progress")
				switch state {
				case "fresh_anchor":
					response = strings.Replace(response, "stored_progress", "fresh_anchor", 1)
				case "parent_closed", "paused":
					response = strings.Replace(response, `"warnings":[]`, fmt.Sprintf(`"warnings":[{"code":%q,"message":"hypothetical only"}]`, state), 1)
				case "missing_next":
					response = strings.Replace(response, `"next":`, `"unknown":`, 1)
				case "wrong_from":
					response = strings.Replace(response, r.URL.Query().Get("from"), "2020-01-01T00:00:00Z", 1)
				case "bad_local":
					response = strings.Replace(response, `"local":"`, `"local":"invalid-`, 1)
				case "unknown_delivery":
					response = strings.Replace(response, `"daemon"`, `"future"`, 1)
				}
				return 200, response
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, true), false)
			if state == "stored_progress" {
				require.Equal(t, "available", got.Diagnostics.Preview.Status)
				require.Len(t, got.Diagnostics.Preview.Next, 1)
			} else {
				require.NotEqual(t, "available", got.Diagnostics.Preview.Status)
				require.Empty(t, got.Diagnostics.Preview.Next)
				require.NotEmpty(t, got.Warnings)
			}
		})
	}
}

func TestCronReceiptInventoryIsSubsequentProjectObservation(t *testing.T) {
	for _, action := range []string{"create", "update", "restore"} {
		for _, state := range []string{"unknown", "warming", "ready", "future"} {
			t.Run(action+"/"+state, func(t *testing.T) {
				daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
					if r.Method == http.MethodPost {
						return 200, receiptPreview(r, "adapter", "adapter_trigger")
					}
					return 200, fmt.Sprintf(`{"inventory":{"state":%q},"warnings":[{"code":"no_live_runtime","message":"no recent advertisements"},{"code":"pin_unmatched","job_uid":%q,"message":"pin absent"},{"code":"choice_unknown","job_uid":%q,"message":"choice absent"},{"code":"adapter_unmatched","job_uid":"another-job","message":"unrelated-private-warning"}]}`, state, receiptJobUID, receiptJobUID)
				})
				got := buildCronReceipt(t.Context(), daemon, 7, action, receiptFixture(t, true, false), false)
				if state == "future" {
					require.Equal(t, "unavailable", got.Diagnostics.RuntimeInventory.Status)
				} else {
					require.Equal(t, "available", got.Diagnostics.RuntimeInventory.Status)
					require.Equal(t, state, got.Diagnostics.RuntimeInventory.InventoryState)
					require.Equal(t, "subsequent_project_inventory", got.Diagnostics.RuntimeInventory.Basis)
				}
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "unrelated-private-warning")
				if state == "ready" {
					require.Contains(t, string(raw), "pin absent")
					if action == "restore" {
						require.NotContains(t, string(raw), "choice absent")
					} else {
						require.Contains(t, string(raw), "choice absent")
					}
				} else {
					require.NotContains(t, string(raw), "pin absent")
				}
			})
		}
	}
}

// receiptJSONEqual compares JSON values without rounding numbers or requiring
// identical whitespace, member order, or string escape spelling.
func receiptJSONEqual(left, right jsontext.Value) bool {
	left = bytes.TrimSpace(left)
	right = bytes.TrimSpace(right)
	if len(left) == 0 || len(right) == 0 || left.Kind() != right.Kind() {
		return false
	}
	switch left.Kind() {
	case '{':
		var a, b map[string]jsontext.Value
		if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, ok := b[key]
			if !ok || !receiptJSONEqual(value, other) {
				return false
			}
		}
		return true
	case '[':
		var a, b []jsontext.Value
		if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !receiptJSONEqual(a[i], b[i]) {
				return false
			}
		}
		return true
	case '"':
		var a, b string
		return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && a == b
	case '0':
		a, okA := new(big.Rat).SetString(string(left))
		b, okB := new(big.Rat).SetString(string(right))
		return okA && okB && a.Cmp(b) == 0
	default:
		return bytes.Equal(left, right)
	}
}

func TestReceiptJSONEqual(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        bool
	}{
		{`{"x":"\u0061","n":1e2}`, ` { "n":100.0, "x":"a" } `, true},
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{`[null,true]`, `[null,true,0]`, false},
		{`{"nested":{}}`, `{"nested":{"extra":1}}`, false},
	} {
		require.Equal(t, tc.want, receiptJSONEqual(jsontext.Value(tc.left), jsontext.Value(tc.right)))
	}
}

// Generated opaque values survive command output, with or without diagnostics.
func TestCronReceiptPreservesOpaqueJSONProperty(t *testing.T) {
	fixture := receiptFixture(t, true, false)
	requestFile := cronCLIFile(t, cronCLIJob(t))
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		number := hegel.Draw(ht, hegel.SampledFrom([]string{"9007199254740993", "1.2300", "9.001e100", "-0.0001"}))
		if hegel.Draw(ht, hegel.Booleans()) {
			number = strconv.FormatInt(hegel.Draw(ht, hegel.Integers[int64](-1<<63, 1<<63-1)), 10)
		}
		action := hegel.Draw(ht, hegel.SampledFrom([]string{"create", "update", "restore"}))
		mode := hegel.Draw(ht, hegel.SampledFrom([]string{"default", "quiet", "unsupported"}))
		encodedValue, err := json.Marshal(value)
		require.NoError(ht, err)
		raw := fmt.Appendf(nil, `%s,"events":[{"kind":"saved"}],"generated":{"nested":[%s,{"number":%s}]}}`, fixture[:len(fixture)-1], encodedValue, number)
		var writes, reads atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/projects":
				_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
			case r.URL.Path == "/api/v1/projects/7":
				_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
			case strings.HasSuffix(r.URL.Path, "/validate") || strings.HasSuffix(r.URL.Path, "/runtimes"):
				reads.Add(1)
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodPut || strings.HasSuffix(r.URL.Path, "/restore"):
				writes.Add(1)
				_, _ = w.Write(raw)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer server.Close()
		args := []string{"job", action}
		if action == "create" {
			args = append(args, "--uid", receiptJobUID)
		} else {
			args = append(args, receiptJobUID, "--expected-event-uid", receiptRevision)
		}
		if action != "restore" {
			args = append(args, "--file", requestFile)
		}
		if mode != "default" {
			args = append(args, "--receipt")
		}
		if mode == "quiet" {
			args = append(args, "--quiet")
		}
		out, _, err := executeCronCLI(t, server.URL, nil, args...)
		require.NoError(ht, err)
		require.Equal(ht, int64(1), writes.Load())
		var fields map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal([]byte(out), &fields))
		var persisted jsontext.Value
		if mode != "default" {
			persisted = fields["persisted"]
			var gotAction string
			require.NoError(ht, json.Unmarshal(fields["action"], &gotAction))
			require.Equal(ht, action, gotAction)
			var receipt cronReceipt
			require.NoError(ht, json.Unmarshal([]byte(out), &receipt))
			status := "unsupported"
			if mode == "quiet" {
				status = "skipped"
			}
			require.Equal(ht, status, receipt.Diagnostics.Preview.Status)
			require.Equal(ht, status, receipt.Diagnostics.RuntimeInventory.Status)
		} else {
			delete(fields, "kata_api_version")
			persisted, err = json.Marshal(fields)
			require.NoError(ht, err)
		}
		require.True(ht, receiptJSONEqual(raw, persisted), "saved response must preserve every JSON value")
		if mode == "unsupported" {
			require.Equal(ht, int64(2), reads.Load())
		} else {
			require.Zero(ht, reads.Load())
		}
	})
}

func TestCronReceiptRejectsRedirectWithoutChangingSharedClient(t *testing.T) {
	calls, redirects := 0, 0
	shared := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		redirects++
		return nil
	}, Transport: receiptTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.NotEqual(t, "/mutation", r.URL.Path)
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"/mutation"}}, Body: io.NopCloser(strings.NewReader("private-redirect-body")), Request: r}, nil
	})}
	got := buildCronReceipt(t.Context(), daemonAPI{baseURL: "http://daemon.example", client: shared}, 7, "create", receiptFixture(t, true, false), false)
	require.Equal(t, 2, calls)
	require.Zero(t, redirects)
	require.Equal(t, "unavailable", got.Diagnostics.Preview.Status)
	require.NoError(t, shared.CheckRedirect(nil, nil))
	require.Equal(t, 1, redirects)
}

func TestCronReceiptRequiresInventoryWireMembers(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"inventory":{"state":"ready"}}`, `{"inventory":{"state":"ready"},"warnings":null}`,
		`{"inventory":{"state":"ready"},"warnings":{}}`, `{"inventory":null,"warnings":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				if r.Method == http.MethodPost {
					return 200, receiptPreview(r, "adapter", "adapter_trigger")
				}
				return 200, body
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, false), false)
			require.Equal(t, "unavailable", got.Diagnostics.RuntimeInventory.Status)
		})
	}
}

func TestCronReceiptAdapterUnknownAnchorAndExhaustion(t *testing.T) {
	for _, basis := range []string{"adapter_defined", "none", "adapter_trigger"} {
		t.Run(basis, func(t *testing.T) {
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				if r.Method == http.MethodGet {
					return 200, `{"inventory":{"state":"unknown"},"warnings":[]}`
				}
				return 200, fmt.Sprintf(`{"valid":true,"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":%q,"zone":"UTC","from":%q,"next":[]}}`, basis, r.URL.Query().Get("from"))
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, false), false)
			require.Empty(t, got.Diagnostics.Preview.Next)
			if basis == "adapter_trigger" {
				require.Equal(t, "available", got.Diagnostics.Preview.Status)
			} else {
				require.Equal(t, "unavailable", got.Diagnostics.Preview.Status)
			}
		})
	}
}

func TestCronReceiptFlagOnlyOnSupportedJobMutations(t *testing.T) {
	want := map[string]bool{"kata cron job create": true, "kata cron job update": true, "kata cron job restore": true}
	seen := map[string]bool{}
	var check func(*cobra.Command)
	check = func(command *cobra.Command) {
		path := command.CommandPath()
		flag := command.Flags().Lookup("receipt")
		if flag == nil {
			flag = command.InheritedFlags().Lookup("receipt")
		}
		require.Equal(t, want[path], flag != nil, "%s", path)
		if want[path] && flag != nil {
			seen[path] = true
		}
		for _, child := range command.Commands() {
			check(child)
		}
	}
	root := newRootCmd()
	require.Nil(t, root.Flags().Lookup("receipt"))
	cron, _, err := root.Find([]string{"cron"})
	require.NoError(t, err)
	require.Equal(t, "cron", cron.Name())
	check(cron)
	require.Equal(t, want, seen)
}

func TestCronReceiptMalformedDiagnosticElementsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message, err := json.Marshal(hegel.Draw(ht, hegel.Text()))
		require.NoError(ht, err)
		invalid := hegel.Draw(ht, hegel.SampledFrom([]string{"null", "{}", `{"code":null,"message":%s}`, `{"message":%s}`, `{"code":"known"}`}))
		invalid = strings.ReplaceAll(invalid, "%s", string(message))
		source := hegel.Draw(ht, hegel.SampledFrom([]string{"preview", "runtime_inventory"}))
		daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
			if r.Method == http.MethodPost {
				response := receiptPreview(r, "adapter", "adapter_trigger")
				if source == "preview" {
					response = strings.Replace(response, `"warnings":[]`, `"warnings":[`+invalid+`]`, 1)
				}
				return 200, response
			}
			return 200, `{"inventory":{"state":"ready"},"warnings":[` + invalid + `]}`
		})
		got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, false), false)
		if source == "preview" {
			require.Equal(ht, "unavailable", got.Diagnostics.Preview.Status)
			require.Empty(ht, got.Diagnostics.Preview.Next)
		} else {
			require.Equal(ht, "unavailable", got.Diagnostics.RuntimeInventory.Status)
		}
	})
}

func TestCronReceiptBoundsServerMessagesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			message = strings.Repeat("é", 513) + message
		}
		r := cronReceipt{Warnings: []cronReceiptWarning{}}
		r.warn("preview", "validation", message)
		require.Len(ht, r.Warnings, 1)
		got := r.Warnings[0].Message
		require.LessOrEqual(ht, len(got), 1024)
		require.True(ht, utf8.ValidString(got))
		require.True(ht, strings.HasPrefix(message, got))
		if len(message) > 1024 {
			require.Greater(ht, len(got), 1020)
		} else {
			require.Equal(ht, message, got)
		}
	})
}

func TestCronReceiptInventoryWarnsAboutConcurrentRevision(t *testing.T) {
	daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost {
			return 200, receiptPreview(r, "adapter", "adapter_trigger")
		}
		return 200, fmt.Sprintf(`{"inventory":{"state":"ready"},"warnings":[{"code":"pin_unmatched","job_uid":%q,"message":"pin absent"}]}`, receiptJobUID)
	})
	got := buildCronReceipt(t.Context(), daemon, 7, "update", receiptFixture(t, true, false), false)
	var caveats []cronReceiptWarning
	for _, warning := range got.Warnings {
		if warning.Code == "revision_unbound" {
			caveats = append(caveats, warning)
		}
	}
	require.Len(t, caveats, 1)
	require.Contains(t, caveats[0].Message, "different definition revision")
}

func TestCronReceiptRejectsInconsistentZone(t *testing.T) {
	for _, zone := range []string{"not-a-zone", "Local", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			daemon := receiptDaemon(t, func(r *http.Request) (int, string) {
				return 200, strings.Replace(receiptPreview(r, "daemon", "stored_progress"), `"zone":"UTC"`, fmt.Sprintf(`"zone":%q`, zone), 1)
			})
			got := buildCronReceipt(t.Context(), daemon, 7, "create", receiptFixture(t, true, true), false)
			require.Equal(t, "unavailable", got.Diagnostics.Preview.Status)
			require.Empty(t, got.Diagnostics.Preview.Next)
		})
	}
}

func TestCronReceiptQuietCommandOutput(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonMode), func(t *testing.T) {
			writes, diagnostics := 0, 0
			raw := receiptFixture(t, true, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
				case "/api/v1/projects/7":
					_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
				default:
					if r.Method == http.MethodPut {
						writes++
						_, _ = w.Write(raw)
					} else {
						diagnostics++
						w.WriteHeader(500)
					}
				}
			}))
			defer server.Close()
			resetFlags(t)
			command := newRootCmd()
			var out, diagnostic bytes.Buffer
			command.SetOut(&out)
			command.SetErr(&diagnostic)
			command.SetContext(contextWithBaseURL(t.Context(), server.URL))
			args := []string{"--project", "spoke-project", "--quiet"}
			if jsonMode {
				args = append(args, "--json")
			}
			args = append(args, "cron", "job", "create", "--uid", receiptJobUID, "--file", cronCLIFile(t, cronCLIJob(t)), "--receipt")
			command.SetArgs(args)
			require.NoError(t, command.Execute())
			require.Equal(t, 1, writes)
			require.Zero(t, diagnostics)
			if jsonMode {
				var fields map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(out.Bytes(), &fields))
				assertReceiptPersisted(t, raw, fields)
				var receipt cronReceipt
				require.NoError(t, json.Unmarshal(out.Bytes(), &receipt))
				require.Equal(t, "skipped", receipt.Diagnostics.Preview.Status)
				require.Equal(t, "skipped", receipt.Diagnostics.RuntimeInventory.Status)
			} else {
				require.Empty(t, out.String())
			}
		})
	}
}

func TestCronReceiptCommandCancellationDuringPostWriteDiagnostic(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writes, diagnostics := 0, 0
	raw := receiptFixture(t, true, false)
	releaseDiagnostic := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		default:
			if r.Method == http.MethodPut {
				writes++
				_, _ = w.Write(raw)
			} else {
				diagnostics++
				cancel() // Only a received successful write starts this read.
				select {
				case <-r.Context().Done():
				case <-releaseDiagnostic:
				}
			}
		}
	}))
	defer server.Close()
	defer close(releaseDiagnostic)
	resetFlags(t)
	command := newRootCmd()
	var out, diagnostic bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostic)
	command.SetContext(contextWithBaseURL(ctx, server.URL))
	command.SetArgs([]string{"--project", "spoke-project", "--json", "cron", "job", "create", "--uid", receiptJobUID, "--file", cronCLIFile(t, cronCLIJob(t)), "--receipt"})
	require.NoError(t, command.Execute())
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, 1, writes)
	require.Equal(t, 1, diagnostics)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(out.Bytes(), &fields))
	assertReceiptPersisted(t, raw, fields)
	var receipt cronReceipt
	require.NoError(t, json.Unmarshal(out.Bytes(), &receipt))
	require.Equal(t, "unavailable", receipt.Diagnostics.Preview.Status)
	require.Equal(t, "unavailable", receipt.Diagnostics.RuntimeInventory.Status)
	require.NotContains(t, diagnostic.String(), "unavailable")
}

func TestCronReceiptRetainsRecognizedErrorsInIncompletePreview(t *testing.T) {
	daemon := receiptDaemon(t, func(*http.Request) (int, string) {
		return 200, `{"valid":false,"errors":[null,{"stage":"ownership","code":"validation","message":"checkpoint absent"}],"warnings":[null]}`
	})
	got := buildCronReceipt(t.Context(), daemon, 7, "restore", receiptFixture(t, true, true), false)
	require.Equal(t, "unavailable", got.Diagnostics.Preview.Status)
	require.Contains(t, fmt.Sprint(got.Warnings), "ownership: checkpoint absent")
	require.Empty(t, got.Diagnostics.Preview.Next)
}

// An incomplete inventory stays unavailable, but one malformed sibling must
// not hide a recognized warning that is relevant to this job and action.
func TestCronReceiptIncompleteInventoryRetainsRelevantWarningsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		state := hegel.Draw(ht, hegel.SampledFrom([]string{"ready", "warming", "unknown"}))
		action := hegel.Draw(ht, hegel.SampledFrom([]string{"create", "update", "restore"}))
		code := hegel.Draw(ht, hegel.SampledFrom([]string{"pin_unmatched", "adapter_unmatched", "choice_unknown", "no_live_runtime"}))
		message := hegel.Draw(ht, hegel.Text())
		matchingJob := hegel.Draw(ht, hegel.Booleans())
		jobUID := receiptJobUID
		if code == "no_live_runtime" && matchingJob {
			jobUID = ""
		} else if !matchingJob {
			jobUID = "01J00000000000000000000009"
		}
		valid, err := json.Marshal(map[string]string{"code": code, "job_uid": jobUID, "message": message})
		require.NoError(ht, err)
		malformed := hegel.Draw(ht, hegel.SampledFrom([]string{
			"null", "{}", "42", "true", `"unknown"`, "[]",
			`{"code":null,"message":"unknown"}`, `{"code":"pin_unmatched"}`,
			`{"code":42,"message":"bad"}`, `{"code":"pin_unmatched","message":false}`,
			`{"code":"pin_unmatched","message":"bad","job_uid":42}`,
			`{"code":"no_live_runtime","message":"bad","job_uid":null}`,
		}))
		warnings := []jsontext.Value{valid, jsontext.Value(malformed)}
		if hegel.Draw(ht, hegel.Booleans()) {
			warnings[0], warnings[1] = warnings[1], warnings[0]
		}
		body, err := json.Marshal(struct {
			Inventory map[string]string `json:"inventory"`
			Warnings  []jsontext.Value  `json:"warnings"`
		}{map[string]string{"state": state}, warnings})
		require.NoError(ht, err)
		r := cronReceipt{Action: action, Warnings: []cronReceiptWarning{}}
		r.readInventory(body, receiptJobUID)
		require.Equal(ht, "unavailable", r.Diagnostics.RuntimeInventory.Status)
		require.Empty(ht, r.Diagnostics.RuntimeInventory.InventoryState, "incomplete data must not advertise ready inventory")
		wantEvidence := state == "ready" && matchingJob && (code != "choice_unknown" || action != "restore")
		var evidence, caveats []cronReceiptWarning
		for _, warning := range r.Warnings {
			if warning.Code == code {
				evidence = append(evidence, warning)
			}
			if warning.Code == "revision_unbound" {
				caveats = append(caveats, warning)
			}
		}
		if wantEvidence {
			require.Len(ht, evidence, 1)
			require.Equal(ht, "runtime_inventory", evidence[0].Source)
			require.True(ht, strings.HasPrefix(message, evidence[0].Message))
			require.LessOrEqual(ht, len(evidence[0].Message), 1024)
			require.True(ht, utf8.ValidString(evidence[0].Message))
			if len(message) <= 1024 {
				require.Equal(ht, message, evidence[0].Message)
			} else {
				_, nextBytes := utf8.DecodeRuneInString(message[len(evidence[0].Message):])
				require.Greater(ht, len(evidence[0].Message)+nextBytes, 1024, "retain the complete prefix that fits the byte budget")
			}
			if code == "no_live_runtime" {
				require.Empty(ht, caveats)
			} else {
				require.Len(ht, caveats, 1)
			}
		} else {
			require.Empty(ht, evidence)
			require.Empty(ht, caveats)
		}
	})
}
