package daemon

import (
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/tracker"
	"strings"
)

// Additional grant locators are derived only from validated stored binding
// config. Semantic write identity and native guards still use SourceKey.
func trackerBindingResourceAlias(binding db.IssueSyncBinding) string {
	switch binding.Provider {
	case "github":
		c, err := githubsync.DecodeConfig(binding.Config)
		if err != nil {
			return ""
		}
		return "github:repository:" + strings.ToLower(c.Host+"/"+c.Owner+"/"+c.Repo)
	case "todoist":
		c, err := todoistsync.DecodeConfig(binding.Config)
		if err != nil {
			return ""
		}
		return "todoist:" + c.APIOrigin + "/project:" + c.ProjectID
	}
	return ""
}
func trackerBindingGrant(d tracker.Descriptor, root, project string, p tracker.Principal, action tracker.Action, binding db.IssueSyncBinding) bool {
	if d.Allows(root, project, p, action, binding.SourceKey) {
		return true
	}
	alias := trackerBindingResourceAlias(binding)
	return alias != "" && d.Allows(root, project, p, action, alias)
}
