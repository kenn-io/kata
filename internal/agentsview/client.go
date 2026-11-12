// Package agentsview reads live usage without persisting upstream data.
package agentsview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/config"
)

// SessionUsage contains only the named facts Kata exposes from AgentsView.
type SessionUsage struct {
	State              string   `json:"state" enum:"priced,unpriced,no_usage,not_found,ambiguous,unauthorized,unavailable"`
	CanonicalSessionID string   `json:"canonical_session_id,omitempty"`
	CostMicrodollars   *int64   `json:"cost_microdollars,omitempty"`
	CostSource         string   `json:"cost_source,omitempty"`
	Models             []string `json:"models,omitempty"`
	UnpricedModels     []string `json:"unpriced_models,omitempty"`
	TotalOutputTokens  *int64   `json:"total_output_tokens,omitempty"`
	SubagentCount      *int64   `json:"subagent_count,omitempty"`
	Candidates         []string `json:"candidates,omitempty"`
}

type Client struct {
	base string
	http *http.Client
}

// NewClient pins its credential to the validated origin and refuses redirects.
func NewClient(cfg config.ResolvedAgentsView, trustPrivate bool) (*Client, error) {
	h := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := (config.BearerPolicy{TrustPrivateNetwork: trustPrivate}).ConfigureClient(h, cfg.URL, cfg.Token); err != nil {
		return nil, err
	}
	return &Client{base: strings.TrimRight(cfg.URL, "/"), http: h}, nil
}

// Lookup makes one bounded-body request. Deadlines and concurrency belong to
// the caller, which can distinguish its request budget from the GET timeout.
func (c *Client) Lookup(ctx context.Context, id string) SessionUsage {
	result, _, _ := c.lookup(ctx, id)
	return result
}

func (c *Client) lookup(ctx context.Context, id string) (SessionUsage, int, bool) {
	unavailable := SessionUsage{State: "unavailable"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/sessions/"+url.PathEscape(id)+"/usage?subagents=true", nil)
	if err != nil {
		return unavailable, 0, false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return unavailable, 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return SessionUsage{State: "unauthorized"}, resp.StatusCode, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return unavailable, resp.StatusCode, false
	}
	var envelope struct {
		Error struct {
			Code       string   `json:"code"`
			Message    string   `json:"message"`
			Candidates []string `json:"candidates"`
		} `json:"error"`
	}
	validEnvelope := json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != ""
	switch resp.StatusCode {
	case http.StatusNotFound:
		if validEnvelope && envelope.Error.Code == "session_not_found" && envelope.Error.Message != "" {
			return SessionUsage{State: "not_found"}, resp.StatusCode, true
		}
		return unavailable, resp.StatusCode, false
	case http.StatusConflict:
		if validEnvelope && envelope.Error.Code == "ambiguous_session_id" && len(envelope.Error.Candidates) <= 5 {
			return SessionUsage{State: "ambiguous", Candidates: envelope.Error.Candidates}, resp.StatusCode, true
		}
		return unavailable, resp.StatusCode, validEnvelope
	case http.StatusOK:
	default:
		return unavailable, resp.StatusCode, validEnvelope
	}
	var v struct {
		SessionID    string `json:"session_id"`
		HasCost      *bool  `json:"has_cost"`
		HasTokenData *bool  `json:"has_token_data"`
		Cost         *struct {
			Microdollars *int64 `json:"microdollars"`
		} `json:"cost"`
		CostSource        string   `json:"cost_source"`
		Models            []string `json:"models"`
		UnpricedModels    []string `json:"unpriced_models"`
		TotalOutputTokens *int64   `json:"total_output_tokens"`
		SubagentCount     *int64   `json:"subagent_count"`
	}
	if json.Unmarshal(body, &v) != nil || v.SessionID == "" || v.HasCost == nil || v.HasTokenData == nil || (v.TotalOutputTokens != nil && *v.TotalOutputTokens < 0) || (v.SubagentCount != nil && *v.SubagentCount < 0) || (v.Cost != nil && v.Cost.Microdollars != nil && *v.Cost.Microdollars < 0) {
		return unavailable, resp.StatusCode, false
	}
	result := SessionUsage{CanonicalSessionID: v.SessionID, Models: v.Models, UnpricedModels: v.UnpricedModels, TotalOutputTokens: v.TotalOutputTokens, SubagentCount: v.SubagentCount}
	switch {
	case *v.HasCost:
		if v.Cost == nil || v.Cost.Microdollars == nil || (v.CostSource != "reported" && v.CostSource != "computed" && v.CostSource != "mixed") {
			return unavailable, resp.StatusCode, false
		}
		result.State = "priced"
		result.CostMicrodollars = v.Cost.Microdollars
		result.CostSource = v.CostSource
	case *v.HasTokenData:
		result.State = "unpriced"
	default:
		result.State = "no_usage"
	}
	return result, resp.StatusCode, true
}

// Probe checks one synthetic identity through the same credential-pinned client.
func (c *Client) Probe(ctx context.Context) string {
	_, status, valid := c.lookup(ctx, "00000000-0000-4000-8000-000000000000")
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "unauthorized"
	case (status == http.StatusNotFound || status == http.StatusOK || status == http.StatusConflict) && valid:
		return "ok"
	case status == 0 || status >= 500 || status >= 300 && status < 400:
		return "unreachable"
	default:
		return "not_agentsview"
	}
}
