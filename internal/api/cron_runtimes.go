package api

import "go.kenn.io/kata/internal/cron"

// BeatCronRuntimeBody is an attributed, non-durable adapter advertisement.
type BeatCronRuntimeBody struct {
	Actor string `json:"actor,omitempty"`
	cron.RuntimeBeat
}

// CronRuntimeRequest scopes a stable runtime installation UID to its project.
type CronRuntimeRequest struct {
	CronProjectRequest
	RuntimeUID string `path:"runtime_uid"`
}

// BeatCronRuntimeRequest replaces the current advertisement for one runtime.
type BeatCronRuntimeRequest struct {
	CronRuntimeRequest
	Body          BeatCronRuntimeBody
	RawBody       []byte
	Authorization string `header:"Authorization"`
}

// BeatCronRuntimeResponse reports local admission and best-effort forwarding separately.
type BeatCronRuntimeResponse struct {
	Body struct {
		Runtime   cron.Runtime `json:"runtime"`
		Forwarded string       `json:"forwarded" enum:"recorded,unavailable,unsupported,not_applicable"`
	}
}

// CronRuntimesResponse contains local observations and advisory warnings.
type CronRuntimesResponse struct{ Body cron.RuntimeInventory }
