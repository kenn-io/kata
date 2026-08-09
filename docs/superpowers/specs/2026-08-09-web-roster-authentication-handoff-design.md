# Web Roster Authentication Handoff Design

## Problem

A fresh browser tab on a daemon that advertises token login cannot reach the
login form. The tab first attempts a local browser session. When that endpoint
is unavailable, the app requests the configured daemon roster. The roster
request returns HTTP 401 and advertises `login`, so the shared credential
transport correctly fences browser authority and selects the login view.

`fetchWebDaemons` then converts every non-success response into the same generic
roster error. The catch block in `loadDaemonRoster` treats that error as a
catalog failure, clears the authentication-required snapshot state, and changes
the shell back to loading mode with a roster retry panel. The retry repeats the
same sequence and leaves the tab at a dead end.

## Scope

Treat only HTTP 401 from the daemon roster as an authentication handoff. Keep
the existing fail-closed roster recovery behavior for empty, malformed,
incompatible, unavailable, and other non-401 roster responses. Do not change
daemon authentication policy, session issuance, roster visibility, target
selection, or persisted state.

## Considered Approaches

### Typed authentication error from the roster client

`fetchWebDaemons` can throw the existing `AuthenticationRequiredError` for HTTP
401 before applying generic response validation. `loadDaemonRoster` can detect
that error and return without clearing state established by the credential
transport.

This is the selected approach. It classifies the response at the transport
boundary, makes the control flow explicit, and does not depend on the timing of
reactive state updates.

### Inspect snapshot state in the roster catch block

The catch block could preserve the current mode when
`snapshots.state.authenticationRequired` is true. This is a smaller edit, but it
couples roster error classification to mutable application state and to the
order in which the credential callback updates that state.

### Return a roster result union

`fetchWebDaemons` could return a union that distinguishes a roster from an
authentication challenge. This is explicit, but it expands the successful
return contract and its call-site handling for a response that already has an
established exception type elsewhere in the web client.

## Design

The daemon roster client will inspect the response status before its existing
generic failure branch. A 401 response will throw
`AuthenticationRequiredError`. Every other non-success response and every
invalid roster body will continue to throw `Configured daemons are
unavailable`.

The application roster loader will catch errors by value. When the error is an
authentication-required error, it will return `false` immediately. It will not
clear snapshots, references, the accepted route, the current shell mode, or the
roster error field. The credential transport remains the single owner of the
authentication transition: it clears tab credentials, fences snapshot
authority, records the requested return path, and selects the login or launch
view from the server-advertised authentication mode.

For all other errors, the current catch path remains unchanged. It clears stale
authority and reference projections, enters loading mode, reports the roster as
unavailable, and offers an explicit retry. This preserves the existing rule
that a missing or invalid catalog must never imply direct access to the host
daemon's local workspace.

## Browser Flow

For a fresh tab on a login-mode daemon:

1. The app starts in loading mode and attempts a local session.
2. The local-session endpoint returns 404 because configured login disables
   direct local sessions.
3. The app requests `/api/v1/ui/daemons`.
4. The response returns 401 with `X-Kata-Web-Authentication: login`.
5. The credential transport records the advertised mode and transitions the
   app to authentication-required login state.
6. The roster client throws `AuthenticationRequiredError`.
7. The roster loader stops without overwriting the authentication transition.
8. The token login form remains visible with the original return path.

After a successful token exchange, the existing login flow invalidates and
reloads the roster with the new browser session before loading selected daemon
authority.

## Error Handling

- Roster HTTP 401: preserve the authentication transition and do not show a
  roster error.
- Other non-success roster responses: show the existing configured-daemons
  recovery panel.
- Empty, malformed, or invalid roster bodies: show the existing recovery panel.
- A roster with no compatible daemon: show the existing compatibility error.
- Local-session bootstrap failure: continue to ordinary authentication
  discovery as before.

## Tests

Add an application regression test that starts with empty tab-local state,
returns 404 from local-session bootstrap, and returns 401 with the `login`
authentication header from the roster. It must observe the login form, no
roster error panel, no roster retry control, and no snapshot request.

Add a roster-client unit test that verifies HTTP 401 rejects with
`AuthenticationRequiredError`. Existing tests for empty and unavailable
rosters continue to prove that non-authentication failures remain fail-closed.

No backend, end-to-end fixture, documentation contract, database schema, or
migration change is required.
