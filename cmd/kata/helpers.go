// Package main is the kata CLI entry point.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// Exit codes per spec §4.7.
const (
	ExitOK            = 0
	ExitInternal      = 1
	ExitUsage         = 2
	ExitValidation    = 3
	ExitNotFound      = 4
	ExitConflict      = 5
	ExitConfirm       = 6
	ExitDaemonUnavail = 7
)

// BodySources is the parsed --body / --body-file / --body-stdin trio. The
// *Set fields capture explicit flag presence (via cmd.Flags().Changed) so
// `--body ""` is treated as a deliberate empty body rather than absent. Stdin
// is its own bool, so presence is implicit.
type BodySources struct {
	Body    string
	BodySet bool
	File    string
	FileSet bool
	Stdin   bool
}

// gitUserFn is a function signature for resolveActor's git fallback so tests
// can inject a stub instead of touching the real `git config user.name`.
type gitUserFn func(context.Context) (string, error)

// resolveBody returns the resolved body text. Mutually exclusive sources;
// returns error otherwise. An explicit empty --body or --body-file is
// honored as a deliberate empty body — only no flag at all returns the
// no-source default.
func resolveBody(b BodySources, stdin io.Reader) (string, error) {
	count := 0
	if b.BodySet {
		count++
	}
	if b.FileSet {
		count++
	}
	if b.Stdin {
		count++
	}
	if count > 1 {
		return "", errors.New("must pass exactly one of --body, --body-file, --body-stdin")
	}
	switch {
	case b.BodySet:
		return b.Body, nil
	case b.FileSet:
		//nolint:gosec // user-supplied path is the whole point of --body-file
		bs, err := os.ReadFile(b.File)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", b.File, err)
		}
		return strings.TrimRight(string(bs), "\n"), nil
	case b.Stdin:
		if stdin == nil {
			stdin = os.Stdin
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, stdin); err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(buf.String(), "\n"), nil
	default:
		return "", nil
	}
}

// resolveActor implements precedence
// flag > KATA_AUTHOR > $USER > git user.name > "anonymous".
// Returns (actor, source) where source is one of
// "flag"|"env"|"user"|"git"|"fallback".
//
// $USER takes precedence over `git config user.name` because login names
// (e.g. "wesm") read more cleanly as event actors and owner tokens than
// display names with spaces (e.g. "Wes McKinney"). Git user.name remains
// in the chain as a final fallback for environments where $USER is unset
// (some sandboxes / cron jobs) but git is configured.
func resolveActor(ctx context.Context, flag string, gitUser gitUserFn) (string, string) {
	if flag != "" {
		return flag, "flag"
	}
	if v := os.Getenv("KATA_AUTHOR"); v != "" {
		return v, "env"
	}
	if v := os.Getenv("USER"); v != "" {
		return v, "user"
	}
	if gitUser == nil {
		gitUser = readGitUserName
	}
	if name, _ := gitUser(ctx); name != "" {
		return name, "git"
	}
	return "anonymous", "fallback"
}

func readGitUserName(ctx context.Context) (string, error) {
	runner := gitcmd.New()
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	out, err := runner.Output(ctx, "", "config", "user.name")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// emitJSON marshals v as a JSON object, splices in "kata_api_version":1 as
// the first key, and writes the result followed by a trailing newline.
//
// The top-level value MUST marshal to a JSON object; non-object payloads
// (scalars, arrays, nil) are rejected. The caller must also not include a
// "kata_api_version" key in v: this helper owns that slot, and silently
// merging a caller-supplied version would let a future struct field strip the
// version stamp on the wire (spec §5.1).
func emitJSON(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return fmt.Errorf("emitJSON: top-level value must be a JSON object, got %T", v)
	}
	// Decode keys structurally — the JSON decoder unescapes \uXXXX sequences,
	// so "kata_api_version" is caught the same as a literal
	// "kata_api_version". A raw bytes.Contains check would miss the escaped
	// form and let the splice produce a duplicate key downstream.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(payload, &keys); err != nil {
		return fmt.Errorf("emitJSON: payload must be a JSON object: %w", err)
	}
	if _, taken := keys["kata_api_version"]; taken {
		return errors.New(`emitJSON: payload must not include "kata_api_version" key`)
	}

	var buf bytes.Buffer
	buf.WriteString(`{"kata_api_version":1`)
	if len(payload) > 2 { // anything other than "{}"
		buf.WriteByte(',')
		buf.Write(payload[1 : len(payload)-1])
	}
	buf.WriteString("}\n")
	_, err = w.Write(buf.Bytes())
	return err
}

// httpDoJSON sends a request body, returns (status, response body bytes).
func httpDoJSON(ctx context.Context, client *http.Client, method, url string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr) //nolint:gosec // daemon targets come from trusted routing; external refs are path-escaped
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	//nolint:gosec // G107: callers in cmd/kata/* always pass daemon-local URLs; this helper is package-internal.
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, bs, nil
}
