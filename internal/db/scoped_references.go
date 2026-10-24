package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// ScopedCommandClassification describes routing, never grants permission.
// Gate names the native policy that remains authoritative at execution.
type ScopedCommandClassification struct{ Disposition, Gate string }

// SourceCommandClass is closed: new API operations must be classified before
// they can participate in scoped commands. CLI and MCP aliases resolve to these
// same operation IDs; unknown IDs have no disposition.
func SourceCommandClass(operationID string) ScopedCommandClassification {
	for _, group := range scopedCommandGroups {
		if slices.Contains(group.operations, operationID) {
			return group.class
		}
	}
	return ScopedCommandClassification{}
}

type scopedCommandGroup struct {
	class      ScopedCommandClassification
	operations []string
}

var scopedCommandGroups = []scopedCommandGroup{
	{ScopedCommandClassification{"source", "native command validation and endpoint claim gates"}, strings.Fields("createIssue editIssue assignIssue unassignIssue setIssuePriority createComment editComment addLabel removeLabel closeIssue reopenIssue createLink deleteLink deleteIssue restoreIssue patchIssueMetadata patchProjectMetadata createCronJob replaceCronJob archiveCronJob restoreCronJob createCronWorkflow replaceCronWorkflow archiveCronWorkflow restoreCronWorkflow observeCronRun")},
	{ScopedCommandClassification{"source", "requireFederatedIssueClaim; notificationMutationActorTx; WithMetadataPatchHook"}, []string{"notifyIssue"}},
	{ScopedCommandClassification{"control", "root claim arbitration; resolveClaimPrincipal"}, strings.Fields("claimIssue acquireIssueLease renewIssueLease releaseIssueLease getIssueLeaseStatus")},
	{ScopedCommandClassification{"current", "caller projection restriction"}, strings.Fields("listAllIssues listIssues showIssue issuePlanningDates showIssueByUID getIssueMetadata reachableIssueGraph listLabels readyIssues readyIssuesGlobal searchIssues readUISnapshot readUIReferences resolveUIIssueReference readUILaunchTarget listProjects showProject showCronJob listCronJobs showCronWorkflow listCronWorkflows getCronCapabilities listRecurrences showRecurrence")},
	{ScopedCommandClassification{"history", "scoped_history_unavailable"}, strings.Fields("pollEvents pollProjectEvents auditCloses digestGlobal digestProject streamEvents showCronRun listCronRuns")},
	{ScopedCommandClassification{"root_only", "ensureFederatedMoveAllowedTx"}, []string{"moveIssue"}},
	{ScopedCommandClassification{"root_only", "ensureFederatedSpokeUnsupportedTx"}, []string{"purgeIssue"}},
	{ScopedCommandClassification{"root_only", "rejectFederatedProjectMerge"}, []string{"mergeProject"}},
	{ScopedCommandClassification{"root_only", "federationBindingRole; ProjectFederatedError"}, []string{"purgeProject"}},
	{ScopedCommandClassification{"source", "ensureProjectWritableTx; requireFederatedImportClaims; normalizeBoundFederationImportActorTx"}, []string{"importIssues"}},
	{ScopedCommandClassification{"root_only", "recurring-origin gate; CheckRecurringMetadataWrite"}, strings.Fields("createRecurrence patchRecurrence deleteRecurrence")},
	{ScopedCommandClassification{"root_only", "host project administration policy"}, strings.Fields("resolveProject initProject removeProject restoreProject detachProjectAlias renameProject rewriteAuthorIdentity")},
	{ScopedCommandClassification{"root_only", "host token/team administration policy"}, strings.Fields("listTokens listTeams showTeam getProjectAccess createToken revokeToken createTeam deleteTeam addTeamMember removeTeamMember setProjectAccess")},
	{ScopedCommandClassification{"root_only", "resolveClaimPrincipal allowEnrollment=false"}, []string{"forceReleaseIssueLease"}},
	{ScopedCommandClassification{"root_only", "host integration administration; external-root adoption gate"}, strings.Fields("getIssueSyncStatus enableIssueSync disableIssueSync runIssueSyncOnce listConnectors getConnectorStatus listConnectorFields getExternalRootBridge mapConnectorField unmapConnectorField bindExternalRoot reconcileExternalRootBridge reconcileExternalRootByKey pauseExternalRootBridge resumeExternalRootBridge resolveExternalField resolveExternalComment unbindExternalRoot")},
	{ScopedCommandClassification{"lifecycle", "host federation policy; live enrollment and relay boundary fences"}, strings.Fields("getFederationStatus getProjectFederation getProjectFederationStatus getFederationBridgeStatus listFederationEnrollments enableProjectFederation skipFederationQuarantine retryFederationQuarantine createFederationEnrollment rotateFederationEnrollment revokeFederationEnrollment createFederationReplica leaveFederationReplica rebindFederationReplica configureFederationSigning connectFederationBridge disconnectFederationBridge repinFederationRootKey")},
	{ScopedCommandClassification{"legacy_transport", "v1 federation auth/signing and relay boundary fences"}, strings.Fields("getFederationProjectMetadata pollFederationProjectEvents disconnectRelayEnrollment ingestFederationProjectEvents getRelayReset offerRelayDeliveries acceptRelayDeliveries ackRelayDeliveries")},
	{ScopedCommandClassification{"service", "host service policy"}, strings.Fields("ping health instance openAPI captureTelemetryEvent doctor")},
}

// ScopedEventClass classifies source references without rewriting source bytes.
// Historical/execution-only records never become current public collections.
func ScopedEventClass(kind string) (string, bool) {
	for class, kinds := range scopedEventGroups {
		if slices.Contains(kinds, kind) {
			return class, true
		}
	}
	return "", false
}

var scopedEventGroups = map[string][]string{
	"current":    strings.Fields("project.metadata_updated issue.updated issue.reopened issue.soft_deleted issue.restored issue.comment_edited issue.assigned issue.unassigned issue.assignment_renewed issue.assignment_expired issue.priority_set issue.priority_cleared issue.labeled issue.unlabeled issue.metadata_updated"),
	"composite":  strings.Fields("issue.created issue.snapshot"),
	"reference":  strings.Fields("issue.linked issue.unlinked issue.links_changed issue.commented issue.closed"),
	"definition": strings.Fields("cron.job.created cron.job.updated cron.job.deleted cron.job.restored cron.job.snapshot cron.workflow.created cron.workflow.updated cron.workflow.deleted cron.workflow.restored cron.workflow.snapshot"),
	"history":    strings.Fields("cron.run.observed cron.run.snapshot claim.acquired claim.renewed claim.released claim.expired claim.force_released claim.violated close.throttled issue.scheduled_reached"),
	"root_only":  strings.Fields("issue.external_root_bound issue.external_root_paused issue.external_root_resumed issue.external_root_unbound issue.external_comment_resolved issue.external_field_conflicted issue.external_field_resolved project.created project.renamed project.merged project.removed project.restored project.alias_removed project.author_rewritten author.rewritten project.federation_enabled project.access_changed issue.moved issue.purged recurrence.created recurrence.updated recurrence.deleted recurrence.materialized recurrence.materialization_skipped"),
}

// ValidateScopedReferences normalizes known machine reference aliases and
// resolves current memberships using authoritative callbacks. It performs no
// reads itself, never infers a reference from free text and grants no authority.
// Historical close evidence and run evidence stay private. Definition-to-
// definition refs require the typed cron validator at admission as well.
func ValidateScopedReferences(event RemoteEvent, resolve func(string) (ScopedEndpoint, error), short func(string, string) (ScopedEndpoint, error)) ([]ScopedEndpoint, error) {
	class, known := ScopedEventClass(event.Type)
	if !known {
		return nil, errors.New("unknown structured scoped family")
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload == nil {
		return nil, errors.New("invalid scoped source payload")
	}
	if err := ValidateFederationEntries(event.Type, event.EventUID, event.Payload); err != nil {
		return nil, err
	}
	result := []ScopedEndpoint{}
	add := func(value string) error {
		if !uid.Valid(value) {
			return errors.New("invalid referenced issue UID")
		}
		endpoint, err := resolve(value)
		if err != nil {
			return err
		}
		if err := endpoint.Validate(); err != nil {
			return err
		}
		if endpoint.IssueUID != value {
			return errors.New("reference resolver changed identity")
		}
		if !slices.Contains(result, endpoint) {
			result = append(result, endpoint)
		}
		return nil
	}
	primary := ""
	if event.IssueUID != nil {
		primary = *event.IssueUID
		if err := add(primary); err != nil {
			return nil, err
		}
		if result[0].ProjectUID != event.ProjectUID {
			return nil, errors.New("primary project mismatch")
		}
	}
	get := func(object map[string]jsontext.Value, key string) (string, error) {
		raw, ok := object[key]
		if !ok {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			return "", fmt.Errorf("invalid reference %s", key)
		}
		return s, nil
	}
	linkEndpointUID := func(a, b string) (string, error) {
		x, err := get(payload, a)
		if err != nil {
			return "", err
		}
		y, err := get(payload, b)
		if err != nil {
			return "", err
		}
		if x != "" && y != "" && x != y {
			return "", errors.New("conflicting reference aliases")
		}
		if x == "" {
			return "", fmt.Errorf("link endpoint must use %s", a)
		}
		return x, nil
	}
	if class == "history" || class == "root_only" {
		return result, nil
	}
	if primary != "" && strings.HasPrefix(event.Type, "issue.") {
		for _, key := range []string{"uid", "issue_uid"} {
			if raw, ok := payload[key]; ok {
				var asserted string
				if json.Unmarshal(raw, &asserted) != nil || asserted != primary {
					return nil, errors.New("payload primary mismatch")
				}
			}
		}
	}
	switch event.Type {
	case "issue.linked", "issue.unlinked":
		from, err := linkEndpointUID("from_uid", "from_issue_uid")
		if err != nil {
			return nil, err
		}
		to, err := linkEndpointUID("to_uid", "to_issue_uid")
		if err != nil {
			return nil, err
		}
		relation, err := get(payload, "type")
		if err != nil {
			return nil, err
		}
		if from == "" || to == "" || from == to || (primary != from && primary != to) || (relation != "parent" && relation != "blocks" && relation != "related") {
			return nil, errors.New("invalid link primary or type")
		}
		peer := to
		if primary == to {
			peer = from
		}
		if event.RelatedIssueUID != nil && *event.RelatedIssueUID != peer {
			return nil, errors.New("related endpoint disagreement")
		}
		if event.Type == "issue.unlinked" {
			a, err := get(payload, "link_from_uid")
			if err != nil {
				return nil, err
			}
			b, err := get(payload, "link_to_uid")
			if err != nil {
				return nil, err
			}
			if (a == "") != (b == "") || (a == "" && relation != "related") || (a != "" && !((a == from && b == to) || (a == to && b == from))) {
				return nil, errors.New("unlink storage endpoint disagreement")
			}
		}
		if err := add(peer); err != nil {
			return nil, err
		}
	case "issue.links_changed":
		if primary == "" {
			return nil, errors.New("atomic references require an envelope subject")
		}
		for _, key := range []string{"parent_set", "parent_removed", "blocks_added", "blocks_removed", "blocked_by_added", "blocked_by_removed", "related_added", "related_removed"} {
			array := !strings.HasPrefix(key, "parent_")
			read := func(name string) ([]string, error) {
				raw, ok := payload[name]
				if !ok {
					return nil, nil
				}
				if array {
					var values []string
					if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &values) != nil {
						return nil, errors.New("reference alias must be array")
					}
					return values, nil
				}
				value, err := get(payload, name)
				if err != nil {
					return nil, err
				}
				return []string{value}, nil
			}
			suffix := "_uid"
			if array {
				suffix = "_uids"
			}
			ids, err := read(key + suffix)
			if err != nil {
				return nil, err
			}
			aliases, err := read(key)
			if err != nil {
				return nil, err
			}
			if ids != nil && aliases != nil && len(ids) != len(aliases) {
				return nil, errors.New("reference alias cardinality mismatch")
			}
			for i, value := range aliases {
				projectUID := event.ProjectUID
				var paired ScopedEndpoint
				if ids != nil {
					// Native atomic edits emit a bare peer ShortID even when
					// the UID belongs to another project. The UID's live
					// membership selects its alias namespace, not the subject.
					var err error
					paired, err = resolve(ids[i])
					if err != nil {
						return nil, err
					}
					if err := paired.Validate(); err != nil {
						return nil, err
					}
					if paired.IssueUID != ids[i] {
						return nil, errors.New("paired UID resolver changed identity")
					}
					projectUID = paired.ProjectUID
				}
				endpoint, err := short(projectUID, value)
				if err != nil {
					return nil, err
				}
				if endpoint.IssueUID == primary {
					return nil, errors.New("self reference")
				}
				if ids != nil && endpoint != paired {
					return nil, errors.New("UID/short reference disagreement")
				}
				if err := add(endpoint.IssueUID); err != nil {
					return nil, err
				}
			}
			for _, value := range ids {
				if value == primary {
					return nil, errors.New("self reference")
				}
				if err := add(value); err != nil {
					return nil, err
				}
			}
		}
		if event.RelatedIssueUID != nil {
			found := false
			for _, r := range result {
				found = found || (r.IssueUID == *event.RelatedIssueUID && r.IssueUID != primary)
			}
			if !found {
				return nil, errors.New("related endpoint not in atomic references")
			}
		}
	case "issue.created", "issue.snapshot":
		var links []struct {
			ToIssueUID string `json:"to_issue_uid"`
			Type       string `json:"type"`
			Incoming   bool   `json:"incoming"`
		}
		if raw, ok := payload["links"]; ok {
			if json.Unmarshal(raw, &links) != nil {
				return nil, errors.New("invalid composite links")
			}
		}
		for _, link := range links {
			if link.ToIssueUID == primary || (link.Type != "parent" && link.Type != "blocks" && link.Type != "related") || (event.Type == "issue.created" && link.Type == "parent" && link.Incoming) {
				return nil, errors.New("invalid composite link")
			}
			if err := add(link.ToIssueUID); err != nil {
				return nil, err
			}
		}
	}

	// Reply targets are comment identities whose resolver yields owning issue
	// membership. Embedded snapshots use the same path as singleton comments.
	replyObjects := []map[string]jsontext.Value{}
	if event.Type == "issue.commented" {
		replyObjects = append(replyObjects, payload)
	}
	if event.Type == "issue.created" || event.Type == "issue.snapshot" {
		if raw, ok := payload["comments"]; ok {
			if err := json.Unmarshal(raw, &replyObjects); err != nil {
				return nil, err
			}
		}
	}
	for _, object := range replyObjects {
		reply := ""
		if raw, ok := object["reply_to_uid"]; ok {
			var nullable *string
			if err := json.Unmarshal(raw, &nullable); err != nil {
				return nil, err
			}
			if nullable != nil {
				reply = *nullable
			}
		}
		if reply == "" {
			continue
		}
		if !uid.Valid(reply) {
			return nil, errors.New("invalid reply comment UID")
		}
		endpoint, err := resolve(reply)
		if err != nil {
			return nil, err
		}
		if err := endpoint.Validate(); err != nil {
			return nil, err
		}
		if event.Type == "issue.commented" && event.RelatedIssueUID != nil && *event.RelatedIssueUID != endpoint.IssueUID {
			return nil, errors.New("reply membership disagreement")
		}
		if !slices.Contains(result, endpoint) {
			result = append(result, endpoint)
		}
	}
	if class == "definition" {
		// Traverse only typed reference fields. Cron options and free text remain
		// authored content, never a source of inferred machine references.
		var definition struct {
			Version int `json:"version"`
			Trigger struct {
				IssueUID string `json:"issue_uid"`
			} `json:"trigger"`
			Issue *struct {
				UID       string `json:"uid"`
				ParentUID string `json:"parent_uid"`
			} `json:"issue"`
			Action struct {
				WorkflowUID string `json:"workflow_uid"`
			} `json:"action"`
		}
		raw, ok := payload["definition"]
		if !ok || json.Unmarshal(raw, &definition) != nil || definition.Version != 1 {
			return nil, errors.New("unknown cron definition version")
		}
		refs := []string{definition.Trigger.IssueUID}
		if definition.Issue != nil {
			refs = append(refs, definition.Issue.UID, definition.Issue.ParentUID)
		}
		for _, value := range refs {
			if value != "" {
				if err := add(value); err != nil {
					return nil, err
				}
			}
		}
		if definition.Action.WorkflowUID != "" && !uid.Valid(definition.Action.WorkflowUID) {
			return nil, errors.New("invalid workflow reference")
		}
		// Definition-to-definition membership is checked by the native cron
		// reference validator; it is not an issue endpoint or publicly passthrough.
	}
	return result, nil
}
