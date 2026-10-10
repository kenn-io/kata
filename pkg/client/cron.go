package client

import "go.kenn.io/kata/internal/uid"

// NewCronUID generates a normalized ULID for a cron definition or run
// observation. Generate it once, persist it with the intended request, and
// reuse both after a timeout. Client calls never replace request identities or
// treat a conflict as a successful create. A new definition or run needs a new
// UID. This function makes no network request.
func NewCronUID() (string, error) { return uid.New() }
