package diagnostics

// Hook describes a currently active daemon hook without its arguments or environment.
type Hook struct {
	Index                     int    `json:"index"`
	Command                   string `json:"command"`
	ExecutableAvailable       bool   `json:"executable_available"`
	WorkingDirectoryAvailable bool   `json:"working_directory_available"`
}

// Hooks contains current availability and aggregate retained history. Historical
// indices never identify today's hooks because a reload may reorder the list.
type Hooks struct {
	Available         bool   `json:"available"`
	Hooks             []Hook `json:"hooks"`
	QueueLength       int    `json:"queue_length"`
	QueueCapacity     int    `json:"queue_capacity"`
	InFlight          int32  `json:"in_flight"`
	Dropped           int64  `json:"dropped"`
	RecentRuns        int    `json:"recent_runs"`
	RecentFailures    int    `json:"recent_failures"`
	HistoryIncomplete bool   `json:"history_incomplete"`
}
