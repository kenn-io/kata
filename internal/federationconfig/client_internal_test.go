package federationconfig

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
)

func TestNewHubClientReportsSanitizedConstructionStage(t *testing.T) {
	tests := []struct {
		name   string
		wantOp string
		marker string
		inject func(error)
	}{
		{
			name: "url", wantOp: "hub URL validation", marker: "url-marker",
			inject: func(injectErr error) {
				normalizeHubURL = func(string, bool) (string, error) { return "", injectErr }
			},
		},
		{
			name: "transport", wantOp: "hub transport setup", marker: "transport-marker",
			inject: func(injectErr error) {
				normalizeHubURL = func(string, bool) (string, error) { return "https://hub.example", nil }
				newHubHTTPClient = func(context.Context, string, client.TargetAuth, client.Opts) (*http.Client, error) {
					return nil, injectErr
				}
			},
		},
		{
			name: "redirect", wantOp: "hub redirect policy", marker: "redirect-marker",
			inject: func(injectErr error) {
				normalizeHubURL = func(string, bool) (string, error) { return "https://hub.example", nil }
				newHubHTTPClient = func(context.Context, string, client.TargetAuth, client.Opts) (*http.Client, error) {
					return &http.Client{}, nil
				}
				configureHubRedirects = func(*http.Client, string) error { return injectErr }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalNormalize := normalizeHubURL
			originalNewHTTPClient := newHubHTTPClient
			originalConfigureRedirects := configureHubRedirects
			t.Cleanup(func() {
				normalizeHubURL = originalNormalize
				newHubHTTPClient = originalNewHTTPClient
				configureHubRedirects = originalConfigureRedirects
			})

			tt.inject(errors.New(tt.marker))
			_, err := NewHubClient(context.Background(), config.CatalogDaemonConfig{
				Name: "primary", URL: "https://url-marker.example/private", Token: "token-marker",
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrHubValidation)
			var hubErr *HubError
			require.True(t, errors.As(err, &hubErr))
			require.NotNil(t, hubErr)
			assert.Equal(t, tt.wantOp, hubErr.Operation)
			for _, secret := range []string{"url-marker", "token-marker", tt.marker} {
				assert.NotContains(t, err.Error(), secret)
			}
			assert.False(t, strings.Contains(err.Error(), "https://"))
		})
	}
}
