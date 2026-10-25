package db

import (
	"encoding/json/v2"

	"go.kenn.io/kata/internal/embedding"
)

// scopedArtifacts reuses retained artifact validation. A valid manifest for
// different (including generated hidden) text is simply ineligible; malformed
// retained manifests for a visible issue are a production error.
func scopedArtifacts(manifests []embedding.ArtifactManifest, issues map[string]ScopedIssueState) ([]embedding.ArtifactManifest, error) {
	result := []embedding.ArtifactManifest{}
	for _, manifest := range manifests {
		issue, ok := issues[manifest.IssueUID]
		if !ok || issue.Deleted || issue.ProjectUID != manifest.ProjectUID {
			continue
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		checked, eligible, err := RelayCheckpointArtifact(issue.ProjectUID, manifest.Digest, string(raw), issue.Title, issue.Body)
		if err != nil {
			return nil, err
		}
		if eligible {
			result = append(result, checked)
		}
	}
	return result, nil
}
