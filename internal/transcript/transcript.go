// Package transcript carries session provenance separately from completion
// evidence. It never reads chat files or contacts a transcript service.
package transcript

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var agentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Transcript identifies an agent and session ID. The optional URL is a
// locator, not a claim that AgentsView has indexed the session.
type Transcript struct {
	Agent     string `json:"agent" required:"true" pattern:"^[a-z0-9][a-z0-9-]*$" maxLength:"32"`
	SessionID string `json:"session_id" required:"true" pattern:"^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$" maxLength:"128"`
	URL       string `json:"url,omitempty"`
}

// Validate rejects unsupported session identifiers and unsafe locators.
func (t Transcript) Validate() error {
	if len(t.Agent) > 32 || !agentPattern.MatchString(t.Agent) {
		return errors.New("transcript agent must be a lowercase alphanumeric or hyphen token starting with a letter or digit, at most 32 characters")
	}
	if !sessionIDPattern.MatchString(t.SessionID) {
		return errors.New("transcript session_id must start with a letter or digit and contain only letters, digits, dots, underscores, or hyphens, at most 128 characters")
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
	if t.Agent != "claude" {
		path += t.Agent + "/"
	}
	// Keep escaped prefixes intact: a configured %2F is part of a segment,
	// not a new path separator. Valid session IDs contain no URL delimiters.
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
