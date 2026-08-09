package api //nolint:revive // package name "api" is the public wire namespace.

// UILaunchTargetRequest identifies one Kata issue by its stable UID.
type UILaunchTargetRequest struct {
	IssueUID string `query:"issue_uid" required:"true" minLength:"26" maxLength:"26"`
}

// UILaunchTargetUnavailableReason explains why Kata cannot provide a safe
// browser destination for an otherwise valid issue.
type UILaunchTargetUnavailableReason string

const (
	// UILaunchTargetBrowserUnavailable means the daemon has no safe,
	// configured browser origin from which to derive a task route.
	UILaunchTargetBrowserUnavailable UILaunchTargetUnavailableReason = "browser_origin_unavailable"
)

// UILaunchTargetResponse contains either an absolute browser URL or a typed
// unavailable state. It never derives browser authority from request headers.
type UILaunchTargetResponse struct {
	Body struct {
		Available bool                            `json:"available"`
		URL       string                          `json:"url,omitempty" format:"uri"`
		Reason    UILaunchTargetUnavailableReason `json:"reason,omitempty" enum:"browser_origin_unavailable"`
	}
}
