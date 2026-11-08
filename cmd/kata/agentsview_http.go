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
	return &cliError{Kind: kindDaemonUnavail, Code: "agentsview_unavailable", ExitCode: ExitDaemonUnavail,
		Message: "AgentsView: pinned discovery/receipt integration is unavailable in this draft; no close or upload attempted"}
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
	cloned := *a.client
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a.client = &cloned
	p := &agentsViewHTTPPort{api: a, projectID: pid, verifier: verifier}
	raw, err := p.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s", pid, url.PathEscape(parsed.RefForAPI)), nil, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
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
	raw, err = p.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", p.projectID), nil, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
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
		return nil, errors.New("AgentsView: daemon response unconfirmed")
	}
	defer response.Body.Close()
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
		return agentsViewSelection{}, errors.New("AgentsView: discovery substituted issue or project")
	}
	selected.Discovery = append(jsontext.Value(nil), raw...)
	return selected, nil
}
func agentsViewEventFromMutation(raw []byte) (*agentsViewCloseEvent, error) {
	var event struct {
		UID        string `json:"uid"`
		Type       string `json:"type"`
		IssueUID   string `json:"issue_uid"`
		ProjectUID string `json:"project_uid"`
		Payload    string `json:"payload"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	var payload struct {
		Association agentsViewAssociation `json:"agentsview_association"`
	}
	if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
		return nil, err
	}
	return &agentsViewCloseEvent{event.UID, event.Type, event.IssueUID, event.ProjectUID, payload.Association}, nil
}
func (p *agentsViewHTTPPort) Close(ctx context.Context, _ string, body map[string]any, headers map[string]string) (agentsViewCloseResult, error) {
	actor, _ := resolveActor(ctx, flags.As, nil)
	body["actor"] = actor
	if len(headers) > 0 {
		body["retry_protocol"] = api.CloseRetryProtocol
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return agentsViewCloseResult{}, err
	}
	copied := map[string]string{"Content-Type": "application/json", "X-Kata-Surface": "cli"}
	for key, value := range headers {
		if key == "Idempotency-Key" || key == "If-Match" {
			copied[key] = value
		}
	}
	response, err := p.request(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", p.projectID, url.PathEscape(p.issueUID)), bytes.NewReader(raw), copied, 10*time.Second)
	if err != nil {
		return agentsViewCloseResult{}, err
	}
	var out struct {
		Changed bool           `json:"changed"`
		Reused  bool           `json:"reused"`
		Event   jsontext.Value `json:"event"`
	}
	if err = json.Unmarshal(response, &out); err != nil {
		return agentsViewCloseResult{}, err
	}
	result := agentsViewCloseResult{Changed: out.Changed, Reused: out.Reused, Raw: response}
	if len(out.Event) > 0 && string(out.Event) != "null" {
		result.Event, err = agentsViewEventFromMutation(out.Event)
	}
	return result, err
}
func (p *agentsViewHTTPPort) Original(ctx context.Context, _ string, eventUID string) (agentsViewCloseEvent, error) {
	var after int64
	for page := 0; page < 128; page++ {
		path := fmt.Sprintf("/api/v1/projects/%d/events?issue=%s&include_effective_close_event_uid=true&limit=10&after_id=%d", p.projectID, url.QueryEscape(p.issueUID), after)
		raw, err := p.request(ctx, http.MethodGet, path, nil, nil, 10*time.Second)
		if err != nil {
			return agentsViewCloseEvent{}, err
		}
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
			var payload struct {
				Association agentsViewAssociation `json:"agentsview_association"`
			}
			if err = json.Unmarshal(event.Payload, &payload); err != nil {
				return agentsViewCloseEvent{}, err
			}
			return agentsViewCloseEvent{event.EventUID, event.Type, *event.IssueUID, event.ProjectUID, payload.Association}, nil
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
		return errors.New("AgentsView: share control exceeds limit")
	}
	response, err := p.request(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/transcript-share", p.projectID, url.PathEscape(p.issueUID)), snapshot, map[string]string{"Content-Type": "application/octet-stream", "X-Kata-Transcript-Control": base64.RawURLEncoding.EncodeToString(raw)}, 120*time.Second)
	if err != nil {
		return err
	}
	return p.verifier.Receipt(ctx, response, c, event)
}
