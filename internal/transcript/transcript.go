// Package transcript carries session provenance separately from completion
// evidence. It never reads chat files or contacts a transcript service.
package transcript

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Transcript identifies a native Claude Code or Codex session. URL is optional
// and is a locator, not a claim that AgentsView has indexed the session.
type Transcript struct {
	Agent     string `json:"agent" enum:"claude,codex" required:"true"`
	SessionID string `json:"session_id" required:"true"`
	URL       string `json:"url,omitempty"`
}

// Validate rejects unsupported session identifiers and unsafe locators.
func (t Transcript) Validate() error {
	if t.Agent != "codex" && t.Agent != "claude" {
		return errors.New("transcript agent must be codex or claude")
	}
	if !sessionIDPattern.MatchString(t.SessionID) {
		return errors.New("transcript session_id must be a native session UUID")
	}
	if t.URL != "" {
		if _, err := parseURL(t.URL); err != nil {
			return err
		}
	}
	return nil
}

// Link follows AgentsView's native-session router (including pg serve).
// Imported archives with host-namespaced IDs need their own verified locator.
func (t Transcript) Link(base string) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	u, err := parseURL(base)
	if err != nil {
		return "", err
	}
	path := "sessions/"
	if t.Agent == "codex" {
		path += "codex/"
	}
	// Keep escaped prefixes intact: a configured %2F is part of a segment,
	// not a new path separator. Session UUIDs contain no URL delimiters.
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + "/" + path + t.SessionID
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return "", errors.New("invalid transcript URL path")
	}
	return u.String(), nil
}

func parseURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		// Never echo an invalid URL: it may contain a credential.
		return nil, errors.New("transcript URL must be absolute HTTP(S) without credentials, query, or fragment")
	}
	return u, nil
}
