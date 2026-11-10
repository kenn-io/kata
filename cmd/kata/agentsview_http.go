package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// Integration seam owned with the discovery/association prerequisites. A real
// implementation verifies discovery and receipts with the already-enrolled root
// pin. An arbitrary key returned alongside a descriptor cannot supply this port.
type agentsViewProofVerifier interface {
	Discovery(context.Context, []byte, string, string) (agentsViewSelection, error)
	Receipt(context.Context, []byte, agentsViewConsent, string) error
}
type agentsViewHTTPPort struct {
	api                  daemonAPI
	projectID            int64
	issueUID, projectUID string
	verifier             agentsViewProofVerifier
}

func agentsViewUnavailable() error {
	return &cliError{
		Kind: kindDaemonUnavail, Code: "agentsview_unavailable", ExitCode: ExitDaemonUnavail,
		Message: "AgentsView: sharing is unavailable in this build; no close or upload attempted",
	}
}

func newLiveAgentsViewWorkflow(ctx context.Context) (*agentsViewWorkflow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// PR1009 and part2 are independently owned and unmerged. Do not substitute
	// unsigned discovery, a receiver response, or a test verifier for their pin.
	return nil, agentsViewUnavailable()
}

// One adapter carries an invocation's selected daemon transport; it never
// forwards credentials to a receiver, follows redirects, or auto-starts services.
func newAgentsViewHTTPPort(ctx context.Context, ref string, verifier agentsViewProofVerifier) (*agentsViewHTTPPort, error) {
	if verifier == nil {
		return nil, agentsViewUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a, err := discoverDaemonAPI(ctx)
	if err != nil {
		return nil, err
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return nil, err
	}
	bare := strings.TrimSpace(flags.Project)
	if bare == "" {
		bare = workspaceProjectName(start)
	}
	parsed, err := ResolveRef(ref, bare)
	if err != nil {
		return nil, err
	}
	pid, _, err := resolveProjectIDAndNameForRef(a, start, parsed.ProjectName, false)
	if err != nil {
		return nil, err
	}
	transport, err := longRunningClientFor(a.ctx, a.baseURL)
	if err != nil {
		return nil, err
	}
	cloned := *transport
	cloned.Transport = agentsViewBoundedResponseTransport{transport.Transport}
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a.client = &cloned
	p := &agentsViewHTTPPort{api: a, projectID: pid, verifier: verifier}
	apiClient, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return nil, err
	}
	issueResponse, callErr := apiClient.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: parsed.RefForAPI}})
	if issueResponse == nil || callErr != nil || issueResponse.StatusCode != http.StatusOK {
		return nil, errors.New("AgentsView: actual issue identity unavailable")
	}
	raw := issueResponse.Body
	var out struct {
		Issue struct {
			UID       string `json:"uid"`
			ProjectID int64  `json:"project_id"`
		} `json:"issue"`
	}
	if err = json.Unmarshal(raw, &out); err != nil || out.Issue.UID == "" || out.Issue.ProjectID <= 0 {
		return nil, errors.New("AgentsView: actual issue identity unavailable")
	}
	p.issueUID = out.Issue.UID
	p.projectID = out.Issue.ProjectID
	projectResponse, callErr := apiClient.ShowProjectWithResponse(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: p.projectID}})
	if projectResponse == nil || callErr != nil || projectResponse.StatusCode != http.StatusOK {
		return nil, errors.New("AgentsView: actual project identity unavailable")
	}
	raw = projectResponse.Body
	var project struct {
		Project struct {
			UID string `json:"uid"`
		} `json:"project"`
	}
	if err = json.Unmarshal(raw, &project); err != nil || project.Project.UID == "" {
		return nil, errors.New("AgentsView: actual project identity unavailable")
	}
	p.projectUID = project.Project.UID
	return p, nil
}

func (p *agentsViewHTTPPort) request(ctx context.Context, method, path string, body io.Reader, headers map[string]string, budget time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.api.baseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if snapshot, ok := body.(*agentsViewFrozenSnapshot); ok {
		request.ContentLength = snapshot.size
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := p.api.client.Do(request)
	if err != nil {
		return nil, &agentsViewSafeFailure{state: "daemon response unconfirmed", cause: err}
	}
	defer func() { _ = response.Body.Close() }()
	headerBytes := 0
	for name, values := range response.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value) + 4
		}
	}
	if headerBytes > 16<<10 || response.Header.Get("Content-Encoding") != "" || response.Uncompressed {
		return nil, errors.New("AgentsView: unsupported daemon response encoding or headers")
	}
	raw, err := io.ReadAll(io.LimitReader(agentsViewContextReader{ctx, response.Body}, agentsViewRecordLimit+1))
	if err != nil || int64(len(raw)) > agentsViewRecordLimit {
		return nil, errors.New("AgentsView: daemon response unavailable or exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Never include a response body or receiver URL; only allowlisted failure
		// codes may enter status. Unknown failures retain their numeric HTTP status.
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &failure)
		switch failure.Code {
		case "close_not_at_origin", "agentsview_route_changed", "agentsview_association_changed", "agentsview_close_changed", "agentsview_unavailable", "agentsview_invalid_association", "agentsview_invalid_control", "agentsview_invalid_snapshot", "agentsview_snapshot_too_large":
			return nil, fmt.Errorf("AgentsView: %s (HTTP %d); retain files, no automatic retry", failure.Code, response.StatusCode)
		}
		return nil, fmt.Errorf("AgentsView: daemon refused request (HTTP %d); no automatic retry", response.StatusCode)
	}
	return raw, nil
}

func (p *agentsViewHTTPPort) Select(ctx context.Context, _ string) (agentsViewSelection, error) {
	raw, err := p.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/transcript-service", p.projectID), nil, nil, 10*time.Second)
	if err != nil {
		return agentsViewSelection{}, err
	}
	selected, err := p.verifier.Discovery(ctx, raw, p.issueUID, p.projectUID)
	if err != nil {
		return selected, err
	}
	if selected.IssueUID != p.issueUID || selected.ProjectUID != p.projectUID {
		return agentsViewSelection{}, errors.New("AgentsView: sharing details name a different issue or project")
	}
	selected.Discovery = append(jsontext.Value(nil), raw...)
	return selected, nil
}

func (p *agentsViewHTTPPort) Close(ctx context.Context, _ string, _ map[string]any, _ map[string]string) (agentsViewCloseResult, error) {
	if err := ctx.Err(); err != nil {
		return agentsViewCloseResult{}, err
	}
	// The generated close body on main has no association/proof fields. Do not
	// round-trip through it and silently omit consent. Connect the real typed
	// operation only after the association prerequisite refreshes its contract.
	return agentsViewCloseResult{}, agentsViewUnavailable()
}

func (p *agentsViewHTTPPort) Original(ctx context.Context, _ string, eventUID string) (agentsViewCloseEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := *p.api.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Transport = agentsViewBoundedResponseTransport{client.Transport}
	apiClient, err := kataclient.NewWithHTTPClient(p.api.baseURL, &client)
	if err != nil {
		return agentsViewCloseEvent{}, err
	}
	var after int64
	for range 128 {
		response, callErr := apiClient.PollProjectEventsWithResponse(ctx, &generated.PollProjectEventsRequestOptions{
			PathParams: &generated.PollProjectEventsPath{ProjectID: p.projectID}, Query: &generated.PollProjectEventsQuery{Issue: &p.issueUID, AfterID: &after, Limit: new(int64(10)), IncludeEffectiveCloseEventUID: new(true)},
		})
		if response == nil || callErr != nil || response.StatusCode != http.StatusOK {
			return agentsViewCloseEvent{}, errors.New("AgentsView: original close history unavailable")
		}
		raw := response.Body
		var history api.PollEventsBody
		if err = json.Unmarshal(raw, &history); err != nil {
			return agentsViewCloseEvent{}, err
		}
		if history.ResetRequired || history.EffectiveCloseEventUID != eventUID {
			return agentsViewCloseEvent{}, errors.New("AgentsView: original close is missing, purged, moved or superseded")
		}
		for _, event := range history.Events {
			if event.EventUID != eventUID {
				continue
			}
			if event.IssueUID == nil {
				break
			}
			association, decodeErr := decodeAgentsViewEventAssociation(event.Payload)
			if decodeErr != nil {
				return agentsViewCloseEvent{}, decodeErr
			}
			return agentsViewCloseEvent{event.EventUID, event.Type, *event.IssueUID, event.ProjectUID, association}, nil
		}
		if len(history.Events) == 0 || history.NextAfterID <= after {
			break
		}
		after = history.NextAfterID
	}
	return agentsViewCloseEvent{}, errors.New("AgentsView: exact original associated close proof unavailable within history limit")
}

func (p *agentsViewHTTPPort) Share(ctx context.Context, _ string, c agentsViewConsent, event string, snapshot io.Reader) error {
	a := c.Association
	control := struct {
		Version         int               `json:"v"`
		ProjectUID      string            `json:"project_uid"`
		AuthorityUID    string            `json:"authority_uid"`
		IssueUID        string            `json:"issue_uid"`
		CloseEventUID   string            `json:"close_event_uid"`
		ServiceID       string            `json:"service_id"`
		Agent           string            `json:"agent"`
		NativeSessionID string            `json:"native_session_id"`
		SessionID       string            `json:"session_id"`
		SHA256          string            `json:"snapshot_sha256"`
		Bytes           int64             `json:"snapshot_bytes"`
		Route           []agentsViewRoute `json:"route"`
		ClientKind      string            `json:"client_kind"`
	}{1, a.ProjectUID, a.AuthorityUID, a.IssueUID, event, a.ServiceID, a.Agent, a.NativeSessionID, a.SessionID, a.SnapshotSHA256, a.SnapshotBytes, a.Route, "cli"}
	raw, err := json.Marshal(control)
	if err != nil || len(raw) > 4096 {
		return errors.New("AgentsView: sharing details too large")
	}
	response, err := p.request(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/transcript-share", p.projectID, url.PathEscape(p.issueUID)), snapshot, map[string]string{"Content-Type": "application/octet-stream", "X-Kata-Transcript-Control": base64.RawURLEncoding.EncodeToString(raw)}, 120*time.Second)
	if err != nil {
		return err
	}
	return p.verifier.Receipt(ctx, response, c, event)
}

// Bound generated-client responses before their normal eager JSON decoder reads
// them. The request context owns the entire header/body budget.
const agentsViewDaemonResponseLimit int64 = 16 << 20

type agentsViewBoundedResponseTransport struct{ http.RoundTripper }

func (t agentsViewBoundedResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport := t.RoundTripper
	if transport == nil {
		transport = http.DefaultTransport
	}
	request = request.Clone(request.Context())
	request.Header.Set("Accept-Encoding", "identity")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	headerBytes := 0
	for name, values := range response.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value) + 4
		}
	}
	if headerBytes > 16<<10 || response.Header.Get("Content-Encoding") != "" || response.Uncompressed {
		_ = response.Body.Close()
		return nil, errors.New("AgentsView: unsupported daemon response encoding or headers")
	}
	raw, err := io.ReadAll(io.LimitReader(agentsViewContextReader{request.Context(), response.Body}, agentsViewDaemonResponseLimit+1))
	_ = response.Body.Close()
	if err != nil || int64(len(raw)) > agentsViewDaemonResponseLimit {
		return nil, errors.New("AgentsView: daemon response exceeds limit or was interrupted")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	return response, nil
}

func decodeAgentsViewEventAssociation(raw []byte) (agentsViewAssociation, error) {
	var payload struct {
		Association jsontext.Value `json:"agentsview_association"`
	}
	var association agentsViewAssociation
	if err := json.Unmarshal(raw, &payload); err != nil {
		return association, errors.New("AgentsView: invalid original close record")
	}
	if err := json.Unmarshal(payload.Association, &association, json.RejectUnknownMembers(true)); err != nil {
		return association, errors.New("AgentsView: original event association unavailable or unsupported")
	}
	return association, nil
}

type agentsViewSafeFailure struct {
	state string
	cause error
}

func (e *agentsViewSafeFailure) Error() string { return "AgentsView: " + e.state }
func (e *agentsViewSafeFailure) Unwrap() error { return e.cause }

func (p *agentsViewHTTPPort) ResolveEvidence(ctx context.Context, evidence []api.Evidence) ([]api.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return expandCloseAttachmentEvidence(ctx, p.api.client, p.api.baseURL, p.projectID, p.issueUID, evidence)
}
