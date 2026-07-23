package client

import (
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/kata/internal/config"
)

const maxOriginPinnedRedirects = 10

// ConfigureOriginPinnedRedirects allows redirects only within baseURL's
// canonical HTTP origin. It applies regardless of bearer authentication
// because request bodies can contain credentials of their own.
func ConfigureOriginPinnedRedirects(httpClient *http.Client, baseURL string) error {
	if httpClient == nil {
		return errors.New("cannot configure redirects on a nil HTTP client")
	}
	origin, err := config.CanonicalHTTPOrigin(baseURL)
	if err != nil {
		return fmt.Errorf("canonicalize redirect origin: %w", err)
	}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		requestOrigin, err := config.CanonicalHTTPOrigin(request.URL.String())
		if err != nil || requestOrigin != origin {
			return errors.New("redirect crossed the configured HTTP origin")
		}
		if len(via) >= maxOriginPinnedRedirects {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return nil
}
