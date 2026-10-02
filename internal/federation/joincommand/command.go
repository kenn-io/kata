// Package joincommand renders portable shell instructions without requiring
// the federation runtime or daemon dependencies.
package joincommand

import (
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// Build returns a shell command for joining the supplied grant, or an empty
// string when the instructions lack the authority or target needed for a join.
func Build(in api.FederationJoinInstructions) string {
	caps, err := db.CanonicalFederationCapabilities(in.Capabilities)
	if err != nil || !slices.Contains(strings.Split(caps, ","), "pull") ||
		in.HubURL == "" || in.ProjectName == "" || in.HubProjectID <= 0 ||
		in.Token == "" || in.Actor == "" ||
		(in.PushEnabled && !slices.Contains(strings.Split(caps, ","), "push")) ||
		(in.AdoptExisting && !in.PushEnabled) {
		return ""
	}
	// Join fetches current metadata with the enrollment token, even if the hub
	// has purged history since these instructions were issued.
	args := []string{
		"kata", "federation", "join", "--project", in.ProjectName, "--hub-url", in.HubURL,
		"--hub-project-id", strconv.FormatInt(in.HubProjectID, 10), "--token", in.Token,
		"--capabilities", strings.ReplaceAll(caps, "claim", "lease"), "--actor", in.Actor,
	}
	if in.PushEnabled {
		args = append(args, "--push")
	}
	if in.AllowInsecure {
		args = append(args, "--allow-insecure")
	}
	if in.AdoptExisting {
		args = append(args, "--adopt-existing")
	}
	for i, arg := range args {
		args[i] = shellQuote(arg)
	}
	return strings.Join(args, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./:=,", r)
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
