package daemon

import (
	"bytes"
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

const federationBatchPollPath = "/api/v1/federation/events:poll"

type federationBatchPollDispatchKey struct{}

type federationSinglePollServedKey struct{}

// federationBatchPollDispatch checks each project in a batch with its own
// single-project poll through the listener's complete handler stack, from the
// batch request as it arrived. Every project is therefore authenticated,
// authorized and fenced by exactly the code that serves the single-project
// route, with nothing shared between projects. The batch returns only whether
// each project is idle, so no event leaves through it.
type federationBatchPollDispatch struct {
	handler http.Handler
	request *http.Request
	signed  bool
}

// withFederationBatchPoll is the outermost layer of a listener that serves
// unsigned federation polls. It records that listener's stack for the batch
// handler and marks single-project polls so they can advertise the batch.
func withFederationBatchPoll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == federationBatchPollPath:
			dispatch := federationBatchPollDispatch{
				handler: next, request: r,
				signed: len(r.Header.Values("Signature")) != 0 || len(r.Header.Values("Signature-Input")) != 0,
			}
			r = r.WithContext(context.WithValue(r.Context(), federationBatchPollDispatchKey{}, dispatch))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") &&
			strings.HasSuffix(r.URL.Path, "/federation/events"):
			r = r.WithContext(context.WithValue(r.Context(), federationSinglePollServedKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

// federationPollEventFeatures is the X-Kata-Event-Features value of a
// single-project poll response. It names the batch route only when the same
// listener serves it to the same unsigned spoke.
func federationPollEventFeatures(ctx context.Context, cfg ServerConfig) string {
	if served, _ := ctx.Value(federationSinglePollServedKey{}).(bool); served && !cfg.FederationSigningRequired {
		return db.SupportedEventFeatures + "," + db.FederationBatchPollFeature
	}
	return db.SupportedEventFeatures
}

func registerFederationBatchPollHandler(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID:  "pollFederationEventsBatch",
		Method:       http.MethodPost,
		Path:         federationBatchPollPath,
		MaxBodyBytes: 64 << 10,
		Summary:      "Check several federated projects for new events with one credential",
		Description: "Checks each listed project through GET /api/v1/projects/{project_id}/federation/events " +
			"exactly as that route would serve the same request, and reports whether it is idle. " +
			"It returns no events: poll a project that is not idle with the single-project route. " +
			"A refusal is that project's error, not the batch's. " +
			"Signed requests and hubs that require signing use the single-project route.",
	}, func(ctx context.Context, in *api.FederationBatchPollRequest) (*api.FederationBatchPollResponse, error) {
		dispatch, ok := ctx.Value(federationBatchPollDispatchKey{}).(federationBatchPollDispatch)
		if !ok || cfg.FederationSigningRequired {
			return nil, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
		if dispatch.signed {
			// No layer verifies a signature over a batch, so accepting one would
			// look like signed transport without being it.
			return nil, api.NewError(http.StatusBadRequest, "validation",
				"signed federation requests must use the single-project poll route", "", nil)
		}
		if token, ok := strings.CutPrefix(in.Authorization, authBearerPrefix); !ok || token == "" {
			return nil, api.NewError(http.StatusUnauthorized, "auth_required", "Authorization bearer required", "", nil)
		}
		seen := make(map[int64]bool, len(in.Body.Projects))
		for _, cursor := range in.Body.Projects {
			if seen[cursor.ProjectID] {
				return nil, api.NewError(http.StatusBadRequest, "validation",
					fmt.Sprintf("project_id %d appears more than once", cursor.ProjectID), "", nil)
			}
			seen[cursor.ProjectID] = true
		}
		pages := make([]api.FederationBatchPollPage, len(in.Body.Projects))
		for i, cursor := range in.Body.Projects {
			page, err := dispatch.probe(cursor)
			if err != nil {
				return nil, internalAPIError(err)
			}
			pages[i] = page
		}
		return &api.FederationBatchPollResponse{
			EventFeatures: db.SupportedEventFeatures + "," + db.FederationBatchPollFeature,
			Body:          api.FederationBatchPollBody{Projects: pages},
		}, nil
	})
}

// probe serves one project's single-project poll for a single event and
// reports its status, and for a 200 whether the project is idle at the cursor.
func (d federationBatchPollDispatch) probe(cursor api.FederationBatchPollCursor) (api.FederationBatchPollPage, error) {
	query := url.Values{"after_id": {strconv.FormatInt(cursor.AfterID, 10)}, "limit": {"1"}}
	request := d.request.Clone(d.request.Context())
	request.Method = http.MethodGet
	request.URL = &url.URL{
		Path:     fmt.Sprintf("/api/v1/projects/%d/federation/events", cursor.ProjectID),
		RawQuery: query.Encode(),
	}
	request.RequestURI = request.URL.RequestURI()
	request.Body, request.GetBody, request.ContentLength = http.NoBody, nil, 0
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Digest", "Accept-Encoding", "Transfer-Encoding"} {
		request.Header.Del(name)
	}
	recorder := &federationBatchPollRecorder{header: http.Header{}}
	d.handler.ServeHTTP(recorder, request)
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	page := api.FederationBatchPollPage{ProjectID: cursor.ProjectID, Status: status}
	raw := recorder.body.Bytes()
	if status == http.StatusOK {
		var body api.PollEventsBody
		if err := jsonv2.Unmarshal(raw, &body); err != nil {
			return api.FederationBatchPollPage{}, fmt.Errorf("decode project %d poll page: %w", cursor.ProjectID, err)
		}
		page.Idle = len(body.Events) == 0 && !body.ResetRequired && body.ResetAfterID == 0 &&
			body.EffectiveCloseEventUID == ""
		if page.Idle {
			page.NextAfterID = body.NextAfterID
		}
		return page, nil
	}
	var envelope api.ErrorEnvelope
	if err := jsonv2.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
		envelope = api.ErrorEnvelope{Status: status, Error: api.ErrorBody{
			Code: "http_error", Message: http.StatusText(status),
		}}
	}
	page.Error = &envelope
	return page, nil
}

// federationBatchPollRecorder keeps one single-project response in memory.
// A probe asks for one event, so the response stays small.
type federationBatchPollRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *federationBatchPollRecorder) Header() http.Header { return r.header }

func (r *federationBatchPollRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *federationBatchPollRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}
