package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func issueSyncEnableAuthority(ctx context.Context, store db.Storage, projectID int64, provider string) error {
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return issueSyncStorageError(err, provider)
	}
	if binding.Role == db.FederationRoleSpoke && binding.Enabled {
		return issueSyncStorageError(db.ErrIssueSyncFederationBinding, provider)
	}
	return nil
}

func issueSyncIntervalSeconds(body api.EnableIssueSyncRequestBody, fallback int, label string) (int, error) {
	interval := strings.TrimSpace(body.Interval)
	present := body.IntervalSecondsPresent || body.IntervalSeconds != 0
	if interval != "" && present {
		return 0, api.NewError(http.StatusBadRequest, "validation", "provide only one "+label+" sync interval form", "", nil)
	}
	if present {
		if body.IntervalSeconds < 1 {
			return 0, api.NewError(http.StatusBadRequest, "validation", label+" sync interval must be at least one second", "", nil)
		}
		return body.IntervalSeconds, nil
	}
	if interval == "" {
		return fallback, nil
	}
	if seconds, err := strconv.Atoi(interval); err == nil {
		if seconds < 1 {
			return 0, api.NewError(http.StatusBadRequest, "validation", label+" sync interval must be at least one second", "", nil)
		}
		return seconds, nil
	}
	duration, err := time.ParseDuration(interval)
	if err != nil || duration < time.Second {
		return 0, api.NewError(http.StatusBadRequest, "validation", label+" sync interval must be at least one second", "", nil)
	}
	return int(duration.Round(time.Second) / time.Second), nil
}
