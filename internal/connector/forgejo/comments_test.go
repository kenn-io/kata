package forgejo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

type nativeComment struct {
	ID      int64     `json:"id"`
	Body    string    `json:"body"`
	User    *user     `json:"user"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

func commentAt(id int64, body string, actor int64) nativeComment {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return nativeComment{ID: id, Body: body, User: &user{ID: actor, Login: "reviewer"}, Created: at, Updated: at}
}
func writeJSON(w http.ResponseWriter, value any) {
	if err := json.MarshalWrite(w, value); err != nil {
		http.Error(w, "invalid fixture response", http.StatusInternalServerError)
	}
}

func TestCommentsSortByCreationAndRevisionsTrackContent(t *testing.T) {
	comments := []nativeComment{commentAt(12, "later", 3), commentAt(9, "mixed-width ID", 3), commentAt(10, "earlier", 4), commentAt(11, "middle", 5)}
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: 8, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	p := connector.ListCommentsParams{RootKey: "23#7@91"}
	a := result[connector.ListCommentsResult](t, exchange(t, h, s.URL, "list_comments", p))
	if len(a.Comments) != 4 || a.Comments[0].ID != "10" || a.Comments[1].ID != "11" || a.Comments[2].ID != "12" || a.Comments[3].ID != "9" {
		t.Fatalf("comments %#v", a)
	}
	comments[2].Body = "externally edited"
	b := result[connector.ListCommentsResult](t, exchange(t, h, s.URL, "list_comments", p))
	if b.Comments[0].ID != a.Comments[0].ID || b.Comments[0].Revision == a.Comments[0].Revision {
		t.Fatal("edit did not retain ID and advance revision")
	}
}
func TestListCommentsAcceptsGhostAndMissingAuthors(t *testing.T) {
	ghost := commentAt(201, "deleted account comment", -1)
	missing := commentAt(202, "comment with missing author", 4)
	missing.User = nil
	comments := []nativeComment{ghost, missing}
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: 8, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	response := exchange(t, New("secret-test-token"), s.URL, "list_comments", connector.ListCommentsParams{RootKey: "23#7@91"})
	if response.Error != nil {
		t.Fatalf("ghost author blocked comment listing: %v", response.Error)
	}
	listed := result[connector.ListCommentsResult](t, response)
	if len(listed.Comments) != 2 || listed.Comments[0].Author.ID != "-1" || listed.Comments[1].Author.ID != "ghost" || listed.Comments[1].Author.DisplayName != "Ghost" {
		t.Fatalf("unexpected ghost author normalization: %#v", listed.Comments)
	}
}

func TestPublicationRetryAfterLostResponseAndScopedStripping(t *testing.T) {
	var comments []nativeComment
	posts := 0
	crash := true
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: 8, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			if r.Method == "POST" {
				posts++
				var p struct {
					Body string `json:"body"`
				}
				if err := json.UnmarshalRead(r.Body, &p); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				c := commentAt(int64(100+posts), p.Body, 8)
				comments = append(comments, c)
				if crash {
					crash = false
					http.Error(w, "secret-provider-error", 500)
					return
				}
				writeJSON(w, c)
				return
			}
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	p := connector.PublishCommentParams{RootKey: "23#7@91", Body: "Exact body\n", OperationID: "operation-example"}
	if r := exchange(t, h, s.URL, "publish_comment", p); r.Error == nil {
		t.Fatal("simulated ambiguous provider error was lost")
	}
	published := result[connector.Comment](t, exchange(t, h, s.URL, "publish_comment", p))
	if posts != 1 || published.ID != "101" || published.Body != p.Body || published.Author.ID != "8" {
		t.Fatalf("retry %#v posts %d", published, posts)
	}
	op := sha256.Sum256([]byte(p.OperationID))
	suffix := ":" + hex.EncodeToString(op[:]) + " -->"
	if !strings.HasSuffix(comments[0].Body, suffix) || !strings.HasPrefix(comments[0].Body, p.Body+"\n\n<!-- kata-publish:") {
		t.Fatalf("provider body %q", comments[0].Body)
	}
	listed := result[connector.ListCommentsResult](t, exchange(t, h, s.URL, "list_comments", connector.ListCommentsParams{RootKey: p.RootKey}))
	if len(listed.Comments) != 1 || listed.Comments[0].Body != p.Body {
		t.Fatalf("listed %#v", listed)
	}
	// A marker from another author remains visible. Text appended after this
	// connector's marker must survive while the marker itself stays hidden.
	comments = append(comments, commentAt(102, comments[0].Body, 9), commentAt(103, comments[0].Body+"\nvisible", 8))
	listed = result[connector.ListCommentsResult](t, exchange(t, h, s.URL, "list_comments", connector.ListCommentsParams{RootKey: p.RootKey}))
	if listed.Comments[1].Body != comments[1].Body || listed.Comments[2].Body != p.Body+"\nvisible" {
		t.Fatalf("other-author body %#v or edited published body %#v", listed.Comments[1].Body, listed.Comments[2].Body)
	}
	raw, err := json.Marshal(map[string]any{"protocol": connector.ProtocolVersion, "id": "other-instance", "instance": "different-instance", "settings": map[string]any{"origin": s.URL}, "method": "list_comments", "params": connector.ListCommentsParams{RootKey: p.RootKey}})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := connector.ServeOne(t.Context(), strings.NewReader(string(raw)), &out, h); err != nil {
		t.Fatal(err)
	}
	var resp connector.Response
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatal(err)
	}
	other := result[connector.ListCommentsResult](t, resp)
	if other.Comments[0].Body != comments[0].Body {
		t.Fatal("stripped another instance's marker")
	}
	// Each conflict is isolated so an earlier unrelated conflict cannot mask it.
	baseBody, publishedBody := p.Body, comments[0].Body
	for _, test := range []struct {
		name     string
		body     string
		comments []nativeComment
	}{
		{name: "different body", body: "different body", comments: []nativeComment{commentAt(101, publishedBody, 8)}},
		{name: "different actor", body: baseBody, comments: []nativeComment{commentAt(102, publishedBody, 9)}},
		{name: "duplicate self deliveries", body: baseBody, comments: []nativeComment{
			commentAt(103, publishedBody, 8), commentAt(104, publishedBody, 8),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p.Body = test.body
			comments = test.comments
			r := exchange(t, h, s.URL, "publish_comment", p)
			if r.Error == nil || r.Error.Code != "publication_conflict" || posts != 1 {
				t.Fatalf("conflict allowed POST: %#v posts %d", r, posts)
			}
		})
	}
}

func TestPublicationRetryFindsDeliveryAfterUnfinishedMarker(t *testing.T) {
	var comments []nativeComment
	posts := 0
	firstResponseLost := true
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: 8, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			if r.Method == "POST" {
				posts++
				var request struct {
					Body string `json:"body"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				created := commentAt(int64(100+posts), request.Body, 8)
				comments = append(comments, created)
				if firstResponseLost {
					firstResponseLost = false
					http.Error(w, "provider response lost", http.StatusInternalServerError)
					return
				}
				writeJSON(w, created)
				return
			}
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	rootKey := "23#7@91"
	scope := (&client{instance: "example-instance"}).scope(rootKey)
	p := connector.PublishCommentParams{
		RootKey: rootKey, Body: "Body with unfinished marker: <!-- kata-publish:" + scope + ":unfinished",
		OperationID: "unfinished-marker-operation",
	}
	if response := exchange(t, h, s.URL, "publish_comment", p); response.Error == nil {
		t.Fatal("simulated lost publication response was hidden")
	}
	got := result[connector.Comment](t, exchange(t, h, s.URL, "publish_comment", p))
	if posts != 1 || got.ID != "101" || got.Body != p.Body {
		t.Fatalf("retry duplicated or missed publication: comment %#v posts %d", got, posts)
	}
}

func TestPublicationMarkerStaysHiddenAfterCredentialAccountChange(t *testing.T) {
	var comments []nativeComment
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var self int64
		switch r.Header.Get("Authorization") {
		case "token account-a":
			self = 8
		case "token account-b":
			self = 9
		default:
			t.Errorf("unexpected authorization %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: self, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			if r.Method == "POST" {
				var request struct {
					Body string `json:"body"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				created := commentAt(101, request.Body, self)
				comments = append(comments, created)
				writeJSON(w, created)
				return
			}
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	params := connector.PublishCommentParams{RootKey: "23#7@91", Body: "Published by the first account", OperationID: "account-switch-publication"}
	if got := result[connector.Comment](t, exchange(t, New("account-a"), server.URL, "publish_comment", params)); got.Body != params.Body {
		t.Fatalf("published comment = %#v", got)
	}
	first := result[connector.ListCommentsResult](t, exchange(t, New("account-a"), server.URL, "list_comments", connector.ListCommentsParams{RootKey: params.RootKey}))
	second := result[connector.ListCommentsResult](t, exchange(t, New("account-b"), server.URL, "list_comments", connector.ListCommentsParams{RootKey: params.RootKey}))
	if len(first.Comments) != 1 || len(second.Comments) != 1 || second.Comments[0].Body != params.Body || second.Comments[0].Revision != first.Comments[0].Revision {
		t.Fatalf("comment changed after account switch: before %#v after %#v", first.Comments, second.Comments)
	}
}

func TestPublicationRetryIgnoresForeignMarkerQuotesButRejectsFormerActorPublication(t *testing.T) {
	var comments []nativeComment
	posts := 0
	self := int64(8)
	s := provider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			writeJSON(w, user{ID: self, Login: "reviewer"})
		case "/api/v1/repositories/23":
			writeJSON(w, repository{ID: 23, FullName: "owner/renamed"})
		case "/api/v1/repos/owner/renamed/issues/7":
			_, _ = w.Write([]byte(issueJSON))
		case "/api/v1/repos/owner/renamed/issues/7/comments":
			if r.Method == "POST" {
				posts++
				var p struct {
					Body string `json:"body"`
				}
				if err := json.UnmarshalRead(r.Body, &p); err != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				comments = append(comments, commentAt(101, p.Body, self))
				http.Error(w, "controlled response loss", 500)
				return
			}
			writeJSON(w, comments)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	h := New("secret-test-token")
	p := connector.PublishCommentParams{RootKey: "23#7@91", Body: "Exact original body", OperationID: "example-quoted-publication"}
	if r := exchange(t, h, s.URL, "publish_comment", p); r.Error == nil || posts != 1 {
		t.Fatalf("expected response loss after publication: %#v posts %d", r, posts)
	}
	comments = append(comments, commentAt(102, "Quoting a prior comment:\n> "+comments[0].Body+"\nMy reply", 9), commentAt(103, "The quoted marker was:\n> "+comments[0].Body, 9))
	got := result[connector.Comment](t, exchange(t, h, s.URL, "publish_comment", p))
	if got.ID != "101" || got.Body != p.Body || posts != 1 {
		t.Fatalf("quoted retry %#v posts %d", got, posts)
	}
	// A current credential cannot adopt or duplicate a genuine publication from
	// the former credential's actor, even though foreign quotations are ignored.
	self = 10
	if r := exchange(t, h, s.URL, "publish_comment", p); r.Error == nil || r.Error.Code != "publication_conflict" || posts != 1 {
		t.Fatalf("former actor publication duplicated: %#v posts %d", r, posts)
	}
	// An externally edited former actor publication keeps its scoped marker.
	// Without a valid self candidate, this ambiguity must forbid creation.
	comments[0].Body = strings.Replace(comments[0].Body, p.Body, "Externally edited prior actor body", 1)
	if r := exchange(t, h, s.URL, "publish_comment", p); r.Error == nil || r.Error.Code != "publication_conflict" || posts != 1 {
		t.Fatalf("edited former actor publication duplicated: %#v posts %d", r, posts)
	}
}
