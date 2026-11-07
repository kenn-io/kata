package mcpserver

import "go.kenn.io/kata/internal/notification"

// parseInboxNotification shares the daemon's notify.* metadata parser so
// current inbox reads and Events matching agree on which requests are valid.
func parseInboxNotification(raw any) (notification.Value, bool) {
	return notification.ParseValue(raw)
}
