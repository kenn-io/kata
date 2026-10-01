package notionsync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func statusSchemaWire() map[string]any {
	ds := groupSchema()
	properties := map[string]any{}
	for _, p := range ds.Properties {
		property := map[string]any{"id": p.ID, "type": p.Type}
		if p.Type == "status" {
			options := []map[string]any{}
			groups := []map[string]any{}
			for _, option := range p.Options {
				options = append(options, map[string]any{"id": option.ID, "name": option.Name})
			}
			for _, group := range p.Groups {
				groups = append(groups, map[string]any{"id": group.ID, "name": group.Name, "option_ids": group.OptionIDs})
			}
			property["status"] = map[string]any{"options": options, "groups": groups}
		}
		properties[p.Name] = property
	}
	return map[string]any{"object": "data_source", "id": sourceID, "parent": map[string]any{"type": "database_id", "database_id": databaseID}, "properties": properties}
}

func statusPageWire(id *string) map[string]any {
	var status any
	if id != nil {
		status = map[string]any{"id": *id, "name": "Custom option"}
	}
	page := pageWire()
	page["properties"].(map[string]any)["Workflow"].(map[string]any)["status"] = status
	return page
}

func TestStatusWriteHasExactPayloadAndVerifiedReadback(t *testing.T) {
	state := "active"
	patches, admissions := 0, 0
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		case "/v1/pages/22222222-2222-4222-8222-222222222222":
			if r.Method == http.MethodPatch {
				patches++
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.JSONEq(t, `{"properties":{"s%3A1":{"status":{"id":"complete-b"}}}}`, string(body))
				state = "complete-b"
			}
			return jsonResponse(t, statusPageWire(&state)), nil
		default:
			t.Fatalf("unexpected provider endpoint %s", r.URL.Path)
			return nil, nil
		}
	})
	config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	s := session(t, c).(StatusSession)
	observed, err := s.WriteStatus(t.Context(), config, "22222222-2222-4222-8222-222222222222", "closed", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, "complete-b", *observed.RawStatus)
	require.Equal(t, 1, patches)
	require.Equal(t, 1, admissions)
}

func TestAmbiguousStatusWriteIsNeverBlindlyRetried(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed503", true: "connection lost"}[transportFailure], func(t *testing.T) {
			state := "active"
			patches := 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					if r.Method == http.MethodPatch {
						patches++
						state = "complete-a"
						if transportFailure {
							wroteHeaders(r)
							return nil, errors.New("test-secret transport diagnostic")
						}
						return response(503, `{"code":"service_unavailable","message":"test-secret private response"}`), nil
					}
					return jsonResponse(t, statusPageWire(&state)), nil
				}
			})
			config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			s := session(t, c).(StatusSession)
			_, err = s.WriteStatus(t.Context(), config, "22222222-2222-4222-8222-222222222222", "closed", func() error { return nil })
			var delivery *issuesync.StatusError
			require.ErrorAs(t, err, &delivery)
			require.True(t, delivery.Ambiguous)
			require.NotContains(t, err.Error(), "test-secret")
			require.Equal(t, 1, patches)
			observed, err := s.WriteStatus(t.Context(), config, "22222222-2222-4222-8222-222222222222", "closed", func() error { return nil })
			require.NoError(t, err)
			require.Equal(t, "complete-a", *observed.RawStatus)
			require.Equal(t, 1, patches)
		})
	}
}

func TestStatusWritePreservesMatchingSubstates(t *testing.T) {
	for _, raw := range []*string{nil, new("active"), new("ready"), new("complete-a")} {
		t.Run(fmtStatusName(raw), func(t *testing.T) {
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, notionOrigin, r.URL.Scheme+"://"+r.URL.Host)
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					return jsonResponse(t, statusPageWire(raw)), nil
				}
			})
			cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			desired := "open"
			if raw != nil && *raw == "complete-a" {
				desired = "closed"
			}
			observed, err := session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, desired, func() error { t.Fatal("no mutation should be admitted"); return nil })
			require.NoError(t, err)
			require.Equal(t, raw, observed.RawStatus)
			require.Equal(t, desired, observed.Status)
			require.Equal(t, testPageID, observed.Locator)
			require.Equal(t, time.Date(2026, 1, 2, 0, 0, 0, 1000000, time.UTC), observed.Version)
		})
	}
}
func fmtStatusName(raw *string) string {
	if raw == nil {
		return "null"
	}
	return *raw
}

func TestStatusWritesRejectUnavailableOrMismatchedPages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"archived", func(p map[string]any) { p["is_archived"] = true }},
		{"trashed", func(p map[string]any) { p["in_trash"] = true }},
		{"missing availability", func(p map[string]any) { delete(p, "is_archived") }},
		{"wrong page", func(p map[string]any) { p["id"] = testUserID }},
		{"moved source", func(p map[string]any) { p["parent"].(map[string]any)["data_source_id"] = databaseID }},
		{"wrong database", func(p map[string]any) { p["parent"].(map[string]any)["database_id"] = otherDatabaseID }},
		{"missing property", func(p map[string]any) { delete(p["properties"].(map[string]any), "Workflow") }},
		{"changed property type", func(p map[string]any) {
			p["properties"].(map[string]any)["Workflow"].(map[string]any)["type"] = "select"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					p := statusPageWire(new("active"))
					tc.change(p)
					return jsonResponse(t, p), nil
				}
			})
			cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			_, err = session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error { t.Fatal("invalid page admitted"); return nil })
			require.Error(t, err)
		})
	}
}

func TestStatusDeliveryAdmissionRunsAfterPacing(t *testing.T) {
	denied := errors.New("binding disabled")
	c, clock := testClient(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		default:
			return jsonResponse(t, statusPageWire(new("active"))), nil
		}
	})
	cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		require.Len(t, clock.waits, 3)
		return denied
	})
	require.ErrorIs(t, err, denied)
}

func TestStatusMutationFailuresAreClassifiedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		body               string
		ambiguous, blocked bool
		delay              time.Duration
	}{
		{"rate limit", 429, `{"code":"rate_limited","message":"test-secret"}`, false, false, 7 * time.Second},
		{"blocked rate limit", 429, `{"code":"rate_limited","additional_data":{"rate_limit_reason":"public_api_request_blocked"},"message":"test-secret"}`, false, true, 0},
		{"permission", 403, `{"code":"restricted_resource","message":"test-secret"}`, false, true, 0},
		{"deleted", 404, `{"code":"object_not_found","message":"test-secret"}`, false, true, 0},
		{"redirect", 302, `test-secret`, false, true, 0},
		{"conflict", 409, `{"code":"conflict_error","message":"test-secret"}`, false, false, 0},
		{"bad success", 200, `test-secret`, true, false, 0},
		{"server error", 503, `{"code":"service_unavailable","message":"test-secret"}`, true, false, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, notionOrigin, r.URL.Scheme+"://"+r.URL.Host)
				if r.Method == http.MethodPatch {
					patches++
					res := response(tc.status, tc.body)
					res.Header.Set("Retry-After", "7")
					res.Header.Set("Location", "https://other.example/private")
					return res, nil
				}
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					return jsonResponse(t, statusPageWire(new("active"))), nil
				}
			})
			cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			_, err = session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error { return nil })
			var delivery *issuesync.StatusError
			require.ErrorAs(t, err, &delivery)
			require.Equal(t, tc.ambiguous, delivery.Ambiguous)
			require.Equal(t, tc.blocked, delivery.Blocked)
			require.Equal(t, tc.delay, delivery.RetryAfter)
			require.NotContains(t, err.Error(), "test-secret")
			require.Equal(t, 1, patches)
		})
	}
}

func TestStatusWriteThatNeverConnectsIsNotAmbiguous(t *testing.T) {
	patches := 0
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		default:
			if r.Method == http.MethodPatch {
				patches++
				return nil, errors.New("dial tcp: connection refused")
			}
			return jsonResponse(t, statusPageWire(new("active"))), nil
		}
	})
	cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.False(t, delivery.Ambiguous, "headers never reached a connection")
	require.Equal(t, 1, patches)
}

func TestStatusWriteCancelledAfterDispatchIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			wroteHeaders(r)
			cancel()
			return nil, ctx.Err()
		}
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		default:
			return jsonResponse(t, statusPageWire(new("active"))), nil
		}
	})
	cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = session(t, c).(StatusSession).WriteStatus(ctx, cfg, testPageID, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.True(t, delivery.Ambiguous)
}

func TestStatusWriteRejectsWrongPatchResponseIdentity(t *testing.T) {
	state := "active"
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		default:
			if r.Method == http.MethodPatch {
				state = "complete-b"
				p := statusPageWire(&state)
				p["id"] = testUserID
				return jsonResponse(t, p), nil
			}
			return jsonResponse(t, statusPageWire(&state)), nil
		}
	})
	cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error { return nil })
	var delivery *issuesync.StatusError
	require.ErrorAs(t, err, &delivery)
	require.True(t, delivery.Ambiguous)
}

func TestStatusReadRefreshesUnknownOptionOnce(t *testing.T) {
	for _, knownOnRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown stays invalid", true: "new option appears"}[knownOnRefresh], func(t *testing.T) {
			schemas, pages := 0, 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					schemas++
					wire := statusSchemaWire()
					if knownOnRefresh && schemas == 2 {
						p := wire["properties"].(map[string]any)["Workflow"].(map[string]any)["status"].(map[string]any)
						p["options"] = append(p["options"].([]map[string]any), map[string]any{"id": "new-option", "name": "New completed substate"})
						groups := p["groups"].([]map[string]any)
						groups[0]["option_ids"] = append(groups[0]["option_ids"].([]string), "new-option")
					}
					return jsonResponse(t, wire), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					pages++
					return jsonResponse(t, statusPageWire(new("new-option"))), nil
				}
			})
			cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			observed, err := session(t, c).(StatusSession).ReadStatus(t.Context(), cfg, testPageID)
			if knownOnRefresh {
				require.NoError(t, err)
				require.Equal(t, "closed", observed.Status)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, 2, schemas)
			require.Equal(t, 1, pages)
		})
	}
}

func TestStatusDeliveryDoesNotRequireUnrelatedContentProperties(t *testing.T) {
	state := "active"
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			wire := statusSchemaWire()
			properties := wire["properties"].(map[string]any)
			for name, raw := range properties {
				property := raw.(map[string]any)
				if property["type"] == "title" {
					property["id"] = "replacement-title"
				} else if property["type"] != "status" {
					delete(properties, name)
				}
			}
			return jsonResponse(t, wire), nil
		case "/v1/databases/" + databaseID:
			return jsonResponse(t, databaseWire()), nil
		default:
			if r.Method == http.MethodPatch {
				state = "complete-b"
			}
			return jsonResponse(t, statusPageWire(&state)), nil
		}
	})
	cfg, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	observed, err := session(t, c).(StatusSession).WriteStatus(t.Context(), cfg, testPageID, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
}

func TestStatusRunSharesValidatedSchemaAcrossSweepReads(t *testing.T) {
	sources, databases, pages := 0, 0, 0
	state := "active"
	c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/data_sources/" + sourceID:
			sources++
			return jsonResponse(t, statusSchemaWire()), nil
		case "/v1/databases/" + databaseID:
			databases++
			return jsonResponse(t, databaseWire()), nil
		default:
			pages++
			return jsonResponse(t, statusPageWire(&state)), nil
		}
	})
	config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	run := &notionStatusRun{session: session(t, c).(StatusSession), config: config}
	mapping := db.IssueStatusMapping{Mapping: db.ImportMapping{ExternalID: "page:22222222-2222-4222-8222-222222222222"}}
	first, err := run.ReadStatus(t.Context(), mapping)
	require.NoError(t, err)
	require.Equal(t, "open", first.Status)
	state = "complete-a"
	second, err := run.ReadStatus(t.Context(), mapping)
	require.NoError(t, err)
	require.Equal(t, "closed", second.Status)
	require.Equal(t, 1, sources)
	require.Equal(t, 1, databases)
	require.Equal(t, 2, pages)
	_, err = run.WriteStatus(t.Context(), mapping, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, 2, sources, "write admission must refresh the schema")
	require.Equal(t, 2, databases)
}

func TestStatusReadPermissionAndBlockedResponsesArePerItemErrors(t *testing.T) {
	for _, status := range []int{403, 404, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				}
				response := jsonResponse(t, map[string]any{"code": "restricted_resource", "additional_data": map[string]any{"rate_limit_reason": "public_api_request_blocked"}})
				response.StatusCode = status
				return response, nil
			})
			config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			_, err = session(t, client).(StatusSession).ReadStatus(t.Context(), config, "22222222-2222-4222-8222-222222222222")
			var classified *issuesync.StatusError
			require.ErrorAs(t, err, &classified)
			require.Truef(t, classified.Blocked, "%#v: %v", classified, err)
			require.False(t, classified.Ambiguous)
		})
	}
}

func TestStatusReadPermanentPageFailuresAreBlocked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"archived page", func(page map[string]any) { page["is_archived"] = true }},
		{"moved page", func(page map[string]any) { page["parent"].(map[string]any)["data_source_id"] = databaseID }},
		{"malformed page", func(page map[string]any) { delete(page, "is_archived") }},
		{"unknown status option", func(page map[string]any) {
			page["properties"].(map[string]any)["Workflow"].(map[string]any)["status"] = map[string]any{"id": "unknown-option"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1/data_sources/" + sourceID:
					return jsonResponse(t, statusSchemaWire()), nil
				case "/v1/databases/" + databaseID:
					return jsonResponse(t, databaseWire()), nil
				default:
					page := statusPageWire(new("active"))
					tc.change(page)
					return jsonResponse(t, page), nil
				}
			})
			config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
			require.NoError(t, err)
			_, err = session(t, client).(StatusSession).ReadStatus(t.Context(), config, testPageID)
			var classified *issuesync.StatusError
			require.ErrorAs(t, err, &classified)
			require.True(t, classified.Blocked)
			require.False(t, classified.Ambiguous)
		})
	}
}

func TestStatusReadSchemaPermissionRemainsRetryable(t *testing.T) {
	client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/data_sources/"+sourceID {
			return response(http.StatusForbidden, `{"code":"restricted_resource"}`), nil
		}
		t.Fatalf("unexpected provider endpoint %s", r.URL.Path)
		return nil, nil
	})
	config, err := ResolveConfig(groupSchema(), Selectors{StatusSync: "two-way"}, "")
	require.NoError(t, err)
	_, err = session(t, client).(StatusSession).ReadStatus(t.Context(), config, testPageID)
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.False(t, classified.Blocked)
	require.False(t, classified.Ambiguous)
}

// wroteHeaders reports a dispatched request the way http.Transport does, so a
// fake transport can simulate a response lost after the server received it.
func wroteHeaders(r *http.Request) {
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
}
