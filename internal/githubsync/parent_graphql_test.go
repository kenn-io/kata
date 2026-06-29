package githubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPFetcherParentDataPaginatesAndIncludesRESTDatabaseIDs(t *testing.T) {
	var requests []parentGraphQLTestRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeParentGraphQLTestRequest(t, r)
		requests = append(requests, request)

		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/graphql", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Contains(t, r.Header.Get("Content-Type"), "application/json")
		assert.Contains(t, request.Query, "repository(owner: $owner, name: $repo)")
		assert.Equal(t, "example-owner", request.Owner)
		assert.Equal(t, "example-repo", request.Repo)

		switch len(requests) {
		case 1:
			assert.Nil(t, request.After)
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"},
							"nodes": [
								{"number": 1, "fullDatabaseId": "101", "parent": {"number": 2, "fullDatabaseId": "102"}},
								{"number": 2, "fullDatabaseId": "102", "parent": null}
							]
						}
					}
				}
			}`)
		case 2:
			require.NotNil(t, request.After)
			assert.Equal(t, "cursor-1", *request.After)
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 3, "fullDatabaseId": "103", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected GraphQL request %d", len(requests))
		}
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Authoritative)
	assert.False(t, data.Unsupported)
	assert.Equal(t, map[int]int64{1: 102}, data.ParentByChild)
	assert.Equal(t, map[int]int64{1: 101, 2: 102, 3: 103}, data.ChildIDByNumber)
	assert.Contains(t, data.ScannedChildren, 1)
	assert.Contains(t, data.ScannedChildren, 2)
	assert.Contains(t, data.ScannedChildren, 3)
	assert.Len(t, requests, 2)
}

func TestHTTPFetcherParentDataReturnsAuthoritativeEmptyForScannedChildrenWithoutParents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"data": {
				"repository": {
					"issues": {
						"pageInfo": {"hasNextPage": false, "endCursor": null},
						"nodes": [
							{"number": 5, "fullDatabaseId": "105", "parent": null},
							{"number": 7, "fullDatabaseId": "107", "parent": null}
						]
					}
				}
			}
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Authoritative)
	assert.False(t, data.Unsupported)
	assert.Empty(t, data.ParentByChild)
	assert.Contains(t, data.ScannedChildren, 5)
	assert.Contains(t, data.ScannedChildren, 7)
}

func TestHTTPFetcherParentDataFeatureUnsupportedIsNonFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"type": "undefinedField",
					"path": ["query", "repository", "issues", "nodes", "parent"],
					"message": "localized schema text"
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Unsupported)
	assert.False(t, data.Authoritative)
	assert.Empty(t, data.ParentByChild)
	assert.Empty(t, data.ScannedChildren)
}

func TestHTTPFetcherParentDataFeatureUnsupportedFromSchemaFieldNameWithoutPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"message": "Field 'fullDatabaseId' doesn't exist on type 'Issue'",
					"extensions": {
						"code": "undefinedField",
						"fieldName": "fullDatabaseId"
					}
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Unsupported)
	assert.False(t, data.Authoritative)
	assert.Empty(t, data.ParentByChild)
	assert.Empty(t, data.ScannedChildren)
}

func TestHTTPFetcherParentDataFeatureUnsupportedFromStructuredClassOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"type": "undefinedField",
					"message": "localized schema text"
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Unsupported)
	assert.False(t, data.Authoritative)
}

func TestHTTPFetcherParentDataFeatureUnsupportedFromStructuredFieldAndValidationClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"message": "localized schema text",
					"extensions": {
						"classification": "GRAPHQL_VALIDATION_FAILED",
						"fieldName": "fullDatabaseId"
					}
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Unsupported)
	assert.False(t, data.Authoritative)
}

func TestHTTPFetcherParentDataFeatureUnsupportedFromMessageOnlySchemaError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"message": "Field 'parent' doesn't exist on type 'Issue'"
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.True(t, data.Unsupported)
	assert.False(t, data.Authoritative)
	assert.Empty(t, data.ParentByChild)
	assert.Empty(t, data.ScannedChildren)
}

func TestHTTPFetcherParentDataAmbiguousMessageOnlyFieldErrorIsFatalAndUncached(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		requests++
		switch requests {
		case 1:
			writeParentGraphQLTestResponse(t, w, `{
				"errors": [
					{
						"message": "The field parent resolver timed out"
					}
				]
			}`)
		case 2:
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 10, "fullDatabaseId": "110", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected ambiguous error request %d", requests)
		}
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	binding := Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}

	_, err := fetcher.ParentData(context.Background(), binding)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field parent resolver timed out")
	assert.Equal(t, 1, requests)

	data, err := fetcher.ParentData(context.Background(), binding)
	require.NoError(t, err)
	assert.True(t, data.Authoritative)
	assert.Contains(t, data.ScannedChildren, 10)
	assert.Equal(t, 2, requests)
}

func TestHTTPFetcherParentDataGraphQLRateLimitRetriesHTTP200Errors(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		attempts++
		switch attempts {
		case 1:
			w.Header().Set("Retry-After", "2")
			writeParentGraphQLTestResponse(t, w, `{
				"errors": [
					{
						"type": "RATE_LIMITED",
						"path": ["repository", "issues"],
						"message": "rate limited"
					}
				]
			}`)
		case 2:
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 4, "fullDatabaseId": "104", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected GraphQL retry attempt %d", attempts)
		}
	}))
	defer server.Close()

	var sleeps []time.Duration
	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	fetcher.graphQLSleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	data, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.Equal(t, 2, attempts)
	assert.Equal(t, []time.Duration{2 * time.Second}, sleeps)
	assert.True(t, data.Authoritative)
	assert.Contains(t, data.ScannedChildren, 4)
}

func TestHTTPFetcherParentDataRetryAfterCapReturnsFatal(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		attempts++
		w.Header().Set("Retry-After", "31")
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"type": "RATE_LIMITED",
					"path": ["repository", "issues"],
					"message": "rate limited"
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	fetcher.graphQLSleep = func(_ context.Context, d time.Duration) error {
		t.Fatalf("unexpected retry sleep %s", d)
		return nil
	}

	_, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)

	assert.Equal(t, 1, attempts)
	assert.Contains(t, err.Error(), "retry wait 31s exceeds")
}

func TestHTTPFetcherParentDataRetryAfterOverflowIsFatal(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		attempts++
		w.Header().Set("Retry-After", "10000000000")
		writeParentGraphQLTestResponse(t, w, `{
			"errors": [
				{
					"type": "RATE_LIMITED",
					"path": ["repository", "issues"],
					"message": "rate limited"
				}
			]
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	fetcher.graphQLSleep = func(_ context.Context, d time.Duration) error {
		t.Fatalf("unexpected retry sleep %s", d)
		return nil
	}

	_, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)

	assert.Equal(t, 1, attempts)
	assert.Contains(t, err.Error(), "exceeds max single sleep")
}

func TestHTTPFetcherParentDataRetryTotalBudgetSpansPages(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeParentGraphQLTestRequest(t, r)
		attempts++
		switch attempts {
		case 1:
			require.Nil(t, request.After)
			w.Header().Set("Retry-After", "30")
			writeParentGraphQLTestResponse(t, w, `{"errors":[{"type":"RATE_LIMITED","path":["repository","issues"],"message":"rate limited"}]}`)
		case 2:
			require.Nil(t, request.After)
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"},
							"nodes": [{"number": 1, "fullDatabaseId": "101", "parent": null}]
						}
					}
				}
			}`)
		case 3:
			require.NotNil(t, request.After)
			assert.Equal(t, "cursor-1", *request.After)
			w.Header().Set("Retry-After", "30")
			writeParentGraphQLTestResponse(t, w, `{"errors":[{"type":"RATE_LIMITED","path":["repository","issues"],"message":"rate limited"}]}`)
		case 4:
			require.NotNil(t, request.After)
			assert.Equal(t, "cursor-1", *request.After)
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": true, "endCursor": "cursor-2"},
							"nodes": [{"number": 2, "fullDatabaseId": "102", "parent": null}]
						}
					}
				}
			}`)
		case 5:
			require.NotNil(t, request.After)
			assert.Equal(t, "cursor-2", *request.After)
			w.Header().Set("Retry-After", "1")
			writeParentGraphQLTestResponse(t, w, `{"errors":[{"type":"RATE_LIMITED","path":["repository","issues"],"message":"rate limited"}]}`)
		default:
			t.Fatalf("unexpected GraphQL retry attempt %d", attempts)
		}
	}))
	defer server.Close()

	var sleeps []time.Duration
	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	fetcher.graphQLSleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	_, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)

	assert.Equal(t, 5, attempts)
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second}, sleeps)
	assert.Contains(t, err.Error(), "exceeds max total sleep")
}

func TestHTTPFetcherParentDataNonAdvancingCursorIsFatal(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeParentGraphQLTestRequest(t, r)
		requests++
		switch requests {
		case 1:
			require.Nil(t, request.After)
		case 2:
			require.NotNil(t, request.After)
			assert.Equal(t, "cursor-1", *request.After)
		default:
			t.Fatalf("unexpected non-advancing cursor request %d", requests)
		}
		writeParentGraphQLTestResponse(t, w, `{
			"data": {
				"repository": {
					"issues": {
						"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"},
						"nodes": [
							{"number": 1, "fullDatabaseId": "101", "parent": null}
						]
					}
				}
			}
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	_, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)

	assert.Equal(t, 2, requests)
	assert.Contains(t, err.Error(), "pagination cursor did not advance")
}

func TestHTTPFetcherParentDataParentWithoutFullDatabaseIDIsFatal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		writeParentGraphQLTestResponse(t, w, `{
			"data": {
				"repository": {
					"issues": {
						"pageInfo": {"hasNextPage": false, "endCursor": null},
						"nodes": [
							{"number": 1, "fullDatabaseId": "101", "parent": {"number": 2}}
						]
					}
				}
			}
		}`)
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")

	_, err := fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent issue 2")
	assert.Contains(t, err.Error(), "fullDatabaseId")
}

func TestHTTPFetcherParentDataRequestUsesOwnerRepoVariablesAllowedByAuthGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeParentGraphQLTestRequest(t, r)
		assert.Equal(t, "/graphql", r.URL.Path)
		assert.Equal(t, "example-owner", request.Owner)
		assert.Equal(t, "example-repo", request.Repo)

		guarded, err := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(request.RawBody))
		require.NoError(t, err)
		ok, err := scopedGraphQLRequest(guarded, Binding{
			Host:  "github.com",
			Owner: "example-owner",
			Repo:  "example-repo",
		})
		require.NoError(t, err)
		assert.True(t, ok)

		writeParentGraphQLTestResponse(t, w, `{
			"data": {
				"repository": {
					"issues": {
						"pageInfo": {"hasNextPage": false, "endCursor": null},
						"nodes": []
					}
				}
			}
		}`)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	rewrite := &rewriteHostRoundTripper{
		target: serverURL,
		next:   server.Client().Transport,
	}

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client: &http.Client{
			Transport: rewrite,
		},
		CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"),
	})

	_, err = fetcher.ParentData(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	require.Len(t, rewrite.seen, 1)
	assert.Equal(t, "https", rewrite.seen[0].Scheme)
	assert.Equal(t, "api.github.com", rewrite.seen[0].Host)
	assert.Equal(t, "/graphql", rewrite.seen[0].Path)
}

func TestHTTPFetcherParentDataEnterpriseRequestUsesOwnerRepoVariablesAllowedByAuthGuard(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeParentGraphQLTestRequest(t, r)
		assert.Equal(t, "/api/graphql", r.URL.Path)
		assert.Equal(t, "example-owner", request.Owner)
		assert.Equal(t, "example-repo", request.Repo)

		guarded, err := http.NewRequest(http.MethodPost, "https://github.example/api/graphql", bytes.NewReader(request.RawBody))
		require.NoError(t, err)
		ok, err := scopedGraphQLRequest(guarded, Binding{
			Host:  "github.example",
			Owner: "example-owner",
			Repo:  "example-repo",
		})
		require.NoError(t, err)
		assert.True(t, ok)

		writeParentGraphQLTestResponse(t, w, `{
			"data": {
				"repository": {
					"issues": {
						"pageInfo": {"hasNextPage": false, "endCursor": null},
						"nodes": []
					}
				}
			}
		}`)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	rewrite := &rewriteHostRoundTripper{
		target: serverURL,
		next:   server.Client().Transport,
	}

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client: &http.Client{
			Transport: rewrite,
		},
		CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"),
	})

	_, err = fetcher.ParentData(context.Background(), Binding{
		Host:  "github.example",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	require.Len(t, rewrite.seen, 1)
	assert.Equal(t, "https", rewrite.seen[0].Scheme)
	assert.Equal(t, "github.example", rewrite.seen[0].Host)
	assert.Equal(t, "/api/graphql", rewrite.seen[0].Path)
}

func TestParentCapabilityCacheReusesUnsupportedAndExpires(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		requests++
		switch requests {
		case 1:
			writeParentGraphQLTestResponse(t, w, `{
				"errors": [
					{
						"type": "undefinedField",
						"path": ["query", "repository", "issues", "nodes", "parent"],
						"message": "localized schema text"
					}
				]
			}`)
		case 2:
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 8, "fullDatabaseId": "108", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected capability probe request %d", requests)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	fetcher.graphQLNow = func() time.Time { return now }
	fetcher.parentCapabilities = newParentCapabilityCache(time.Hour, fetcher.graphQLNow)
	binding := Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"}

	first, err := fetcher.ParentData(context.Background(), binding)
	require.NoError(t, err)
	assert.True(t, first.Unsupported)
	assert.Equal(t, 1, requests)

	second, err := fetcher.ParentData(context.Background(), binding)
	require.NoError(t, err)
	assert.True(t, second.Unsupported)
	assert.Equal(t, 1, requests)

	now = now.Add(time.Hour + time.Nanosecond)
	third, err := fetcher.ParentData(context.Background(), binding)
	require.NoError(t, err)
	assert.True(t, third.Authoritative)
	assert.False(t, third.Unsupported)
	assert.Contains(t, third.ScannedChildren, 8)
	assert.Equal(t, 2, requests)
}

func TestParentCapabilityCacheDoesNotCacheTransientErrors(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeParentGraphQLTestRequest(t, r)
		requests++
		switch requests {
		case 1:
			writeParentGraphQLTestResponse(t, w, `{
				"errors": [
					{
						"type": "FORBIDDEN",
						"path": ["repository", "issues"],
						"message": "resource not accessible"
					}
				]
			}`)
		case 2:
			writeParentGraphQLTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 9, "fullDatabaseId": "109", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected transient retry request %d", requests)
		}
	}))
	defer server.Close()

	fetcher := newParentGraphQLTestFetcher(server.URL + "/graphql")
	binding := Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}

	_, err := fetcher.ParentData(context.Background(), binding)
	require.Error(t, err)
	assert.Equal(t, 1, requests)

	data, err := fetcher.ParentData(context.Background(), binding)
	require.NoError(t, err)
	assert.True(t, data.Authoritative)
	assert.Contains(t, data.ScannedChildren, 9)
	assert.Equal(t, 2, requests)
}

type parentGraphQLTestRequest struct {
	Query   string
	Owner   string
	Repo    string
	After   *string
	RawBody []byte
}

func newParentGraphQLTestFetcher(graphQLURL string) *HTTPFetcher {
	return NewHTTPFetcher(HTTPFetcherConfig{
		Client:             http.DefaultClient,
		CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"),
		GraphQLURLOverride: graphQLURL,
	})
}

func writeParentGraphQLTestResponse(t testing.TB, w http.ResponseWriter, body string) {
	t.Helper()
	_, err := fmt.Fprint(w, body)
	require.NoError(t, err)
}

func decodeParentGraphQLTestRequest(t *testing.T, r *http.Request) parentGraphQLTestRequest {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)

	var payload graphQLRequestBody
	require.NoError(t, json.Unmarshal(body, &payload))
	owner, ok := graphQLStringVariable(payload.Variables, "owner")
	require.True(t, ok)
	repo, ok := graphQLStringVariable(payload.Variables, "repo")
	require.True(t, ok)
	rawAfter, ok := payload.Variables["after"]
	require.True(t, ok)
	var after *string
	require.NoError(t, json.Unmarshal(rawAfter, &after))

	if strings.Contains(payload.Query, "\r") {
		t.Fatalf("GraphQL query contains CRLF")
	}
	return parentGraphQLTestRequest{
		Query:   payload.Query,
		Owner:   owner,
		Repo:    repo,
		After:   after,
		RawBody: body,
	}
}
