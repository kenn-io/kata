package forgejo

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.kenn.io/kata/pkg/connector"
)

func exchange(t *testing.T, h connector.Handler, origin, method string, params any) connector.Response {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"protocol": connector.ProtocolVersion, "id": "test-call", "instance": "example-instance", "settings": map[string]any{"origin": origin}, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := connector.ServeOne(context.Background(), bytes.NewReader(raw), &out, h); err != nil {
		t.Fatal(err)
	}
	var response connector.Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
func result[T any](t *testing.T, response connector.Response) T {
	t.Helper()
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	var value T
	if err := json.Unmarshal(response.Result, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func provider(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret-test-token" {
			t.Errorf("missing provider authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		fn(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

const issueJSON = `{"id":91,"number":7,"title":"Example issue","body":"Example body","state":"open","updated_at":"2026-10-01T12:00:00Z","user":{"id":4,"login":"author"},"repository":{"id":23,"full_name":"owner/renamed"}}`

func TestDescribeAuthenticatesAndKeepsInvocationSettings(t *testing.T) {
	a := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":8,"login":"reviewer"}`))
	})
	b := provider(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":9,"login":"second"}`)) })
	h := New("secret-test-token")
	for _, tc := range []struct{ origin, id string }{{a.URL, "8"}, {b.URL, "9"}} {
		d := result[connector.Description](t, exchange(t, h, tc.origin, "describe", connector.DescribeParams{}))
		if d.ConnectorID != "forgejo" || d.AccountIdentity != tc.origin || d.SelfActorID != tc.id {
			t.Fatalf("description %#v", d)
		}
		for _, cap := range d.Capabilities {
			if cap == connector.CapabilityConditionalFields {
				t.Fatal("stock provider cannot promise atomic CAS")
			}
		}
	}
}
func TestSettingsAndLocatorBoundaries(t *testing.T) {
	for _, origin := range []string{"http://forge.example", "https://user:pass@forge.example", "https://forge.example/prefix", "https://forge.example?secret=yes", "https://forge.example#fragment", "https://forge.example#", "https://forge.example:bad"} {
		r := exchange(t, New("secret-test-token"), origin, "describe", connector.DescribeParams{})
		if r.Error == nil || r.Error.Code != "invalid_settings" {
			t.Fatalf("origin %q: %#v", origin, r)
		}
		if origin == "http://forge.example" && !strings.Contains(r.Error.Message, "literal loopback, private, link-local, or CGNAT IP address") {
			t.Fatalf("HTTP policy error %q", r.Error.Message)
		}
	}
	calls := 0
	s := provider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "secret-test-token leaked-provider-body", 500)
	})
	for _, locator := range []string{"https://elsewhere.example/owner/repo/issues/7", "owner/repo#07", "owner/../repo#7", "owner/repo#0", s.URL + "/owner/repo/issues/7?x=y", s.URL + "/owner/repo/issues/7#fragment", s.URL + "/owner/repo/issues/7#", s.URL + "/owner%2Frepo/repo/issues/7"} {
		r := exchange(t, New("secret-test-token"), s.URL, "resolve_root", connector.ResolveRootParams{Locator: locator})
		if r.Error == nil {
			t.Fatalf("accepted %q", locator)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid locators reached provider %d times", calls)
	}
}

func TestFullIssueLocatorNormalizesOrigin(t *testing.T) {
	c := &client{origin: "https://forgejo.example"}
	for _, locator := range []string{
		"https://FORGEJO.EXAMPLE/owner/repo/issues/7",
		"https://forgejo.example:443/owner/repo/issues/7",
	} {
		ref, err := c.locator(locator)
		if err != nil || ref != (issueRef{owner: "owner", repo: "repo", number: 7}) {
			t.Errorf("locator %q: ref=%+v err=%v", locator, ref, err)
		}
	}
	for _, locator := range []string{
		"https://other.example/owner/repo/issues/7",
		"https://forgejo.example:444/owner/repo/issues/7",
	} {
		if _, err := c.locator(locator); err == nil {
			t.Errorf("accepted cross-origin locator %q", locator)
		}
	}
}

func TestRedirectAndProviderErrorsAreSafe(t *testing.T) {
	escaped := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { escaped++; _, _ = w.Write([]byte(`{"id":8}`)) }))
	defer target.Close()
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret-path", http.StatusFound)
	})
	response := exchange(t, New("secret-test-token"), s.URL, "describe", connector.DescribeParams{})
	if response.Error == nil || escaped != 0 {
		t.Fatalf("redirect escaped: %d, %#v", escaped, response)
	}
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret-test-token private-provider-body", 500)
	})
	response = exchange(t, New("secret-test-token"), s.URL, "describe", connector.DescribeParams{})
	if response.Error == nil || !strings.Contains(response.Error.Message, "500") || strings.Contains(response.Error.Message, "secret") || strings.Contains(response.Error.Message, "private") {
		t.Fatalf("unsafe error %#v", response.Error)
	}
}
func TestImmutableRootsAndVerifiedCompletion(t *testing.T) {
	body := issueJSON
	patches := 0
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/original", "/api/v1/repositories/23":
			_, _ = w.Write([]byte(`{"id":23,"full_name":"owner/renamed"}`))
		case "/api/v1/repos/owner/renamed/issues/7":
			if r.Method == "PATCH" {
				patches++
				var v map[string]any
				if err := json.UnmarshalRead(r.Body, &v); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				if v = appliedOptions(v); len(v) != 1 || v["state"] != "closed" {
					t.Errorf("close payload %#v", v)
				}
				body = strings.Replace(body, `"state":"open"`, `"state":"closed"`, 1)
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	resolved := result[connector.Root](t, exchange(t, h, s.URL, "resolve_root", connector.ResolveRootParams{Locator: "owner/original#7"}))
	if resolved.Key != "23#7@91" || resolved.IdentityKey != s.URL || resolved.State != "open" || resolved.Actor != nil {
		t.Fatalf("root %#v", resolved)
	}
	again := result[connector.Root](t, exchange(t, h, s.URL, "read_root", connector.ReadRootParams{RootKey: resolved.Key}))
	if resolved.Revision != again.Revision {
		t.Fatal("observation changed content revision")
	}
	complete := result[connector.Root](t, exchange(t, h, s.URL, "complete_root", connector.CompleteRootParams{RootKey: resolved.Key}))
	if complete.State != "complete" || complete.Revision == resolved.Revision || complete.Actor != nil || patches != 1 {
		t.Fatalf("closure %#v patches %d", complete, patches)
	}
	result[connector.Root](t, exchange(t, h, s.URL, "complete_root", connector.CompleteRootParams{RootKey: resolved.Key}))
	if patches != 1 {
		t.Fatal("closed issue rewritten")
	}
	body = strings.Replace(body, `"id":91`, `"id":92`, 1)
	if r := exchange(t, h, s.URL, "read_root", connector.ReadRootParams{RootKey: resolved.Key}); r.Error == nil {
		t.Fatal("replacement issue accepted")
	}
	if r := exchange(t, h, s.URL, "complete_root", connector.CompleteRootParams{RootKey: resolved.Key}); r.Error == nil || patches != 1 {
		t.Fatal("replacement issue mutated")
	}
	body = strings.Replace(issueJSON, `"state":"open"`, `"state":"open","pull_request":{}`, 1)
	if r := exchange(t, h, s.URL, "read_root", connector.ReadRootParams{RootKey: resolved.Key}); r.Error == nil {
		t.Fatal("pull request accepted")
	}
}
func TestClosedRootDoesNotAttributeItsAuthorAsCloser(t *testing.T) {
	body := strings.Replace(issueJSON, `"state":"open"`, `"state":"closed","closed_at":"2026-10-01T12:30:00Z"`, 1)
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repositories/23" {
			_, _ = w.Write([]byte(`{"id":23,"full_name":"owner/renamed"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	})

	root := result[connector.Root](t, exchange(t, New("secret-test-token"), s.URL, "read_root", connector.ReadRootParams{RootKey: "23#7@91"}))

	if root.State != "complete" || root.Actor != nil {
		t.Fatalf("closed root attributed its issue author as the transition actor: %#v", root)
	}
}

func TestClosureRequiresClosedReadback(t *testing.T) {
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repositories/23" {
			_, _ = w.Write([]byte(`{"id":23,"full_name":"owner/renamed"}`))
			return
		}
		_, _ = w.Write([]byte(issueJSON))
	})
	if r := exchange(t, New("secret-test-token"), s.URL, "complete_root", connector.CompleteRootParams{RootKey: "23#7@91"}); r.Error == nil {
		t.Fatal("open readback accepted as complete")
	}
}

func TestMissingRootsPauseAndFieldsAdvanceRevision(t *testing.T) {
	missing := false
	body := strings.TrimSuffix(issueJSON, "}") + `,"labels":[{"id":2}]}`
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if missing {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/v1/repositories/23" {
			_, _ = w.Write([]byte(`{"id":23,"full_name":"owner/renamed"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	})
	h := New("secret-test-token")
	p := connector.ReadRootParams{RootKey: "23#7@91"}
	a := result[connector.Root](t, exchange(t, h, s.URL, "read_root", p))
	body = strings.Replace(body, `"labels":[{"id":2}]`, `"labels":[{"id":15}]`, 1)
	b := result[connector.Root](t, exchange(t, h, s.URL, "read_root", p))
	if a.Revision == b.Revision || b.Fields["labels"].Kind != "string_set" {
		t.Fatal("field content omitted from snapshot revision")
	}
	missing = true
	r := exchange(t, h, s.URL, "read_root", p)
	if r.Error == nil || r.Error.Code != "root_not_found" {
		t.Fatalf("missing root is not pauseable %#v", r.Error)
	}
}
