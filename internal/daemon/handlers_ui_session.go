package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"go.kenn.io/kata/internal/api"
)

type uiLocalSessionRequest = api.UILocalSessionRequest
type uiLoginRequest = api.UILoginRequest
type uiSessionResponse = api.UISessionResponse

func registerUISessionHandlers(mux *http.ServeMux, manager *WebSessionManager) {
	mux.HandleFunc(http.MethodPost+" /api/v1/ui/session/login", func(w http.ResponseWriter, r *http.Request) {
		var input uiLoginRequest
		if err := decodeUIJSON(r, &input); err != nil {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", "invalid login request")
			return
		}
		issued, err := manager.Login(r.Context(), input.Token, input.ReturnPath)
		if err != nil {
			api.WriteEnvelope(w, http.StatusUnauthorized, "login_invalid", "login token invalid")
			return
		}
		issueHTTPSession(w, manager, issued)
	})

	mux.HandleFunc(http.MethodPost+" /api/v1/ui/session/local", func(w http.ResponseWriter, r *http.Request) {
		var input uiLocalSessionRequest
		if err := decodeUIJSON(r, &input); err != nil {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", "invalid local session request")
			return
		}
		issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, input.ReturnPath)
		if err != nil {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", "invalid local session request")
			return
		}
		issueHTTPSession(w, manager, issued)
	})

	mux.HandleFunc(http.MethodDelete+" /api/v1/ui/session", func(w http.ResponseWriter, r *http.Request) {
		manager.Logout(r.Header.Get(webSessionHeader))
		w.WriteHeader(http.StatusNoContent)
	})
}

func issueHTTPSession(w http.ResponseWriter, manager *WebSessionManager, issued IssuedWebSession) {
	http.SetCookie(w, manager.Cookie(issued.Cookie))
	actorPolicy := "request"
	if issued.Principal.Actor != "" {
		actorPolicy = "identity"
	}
	writeUIJSON(w, http.StatusOK, uiSessionResponse{
		Session: issued.Session, CSRF: issued.CSRF, ReturnPath: issued.ReturnPath,
		Writable: issued.Writable, Updates: issued.Updates, ActorPolicy: actorPolicy,
	})
}

func decodeUIJSON(r *http.Request, target any) error {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeUIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
