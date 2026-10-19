package issuesync

import "errors"

// PublicationValidation identifies a locally established, safe publication
// failure. Messages and remedies come only from the catalog below.
type PublicationValidation string

const (
	// PublicationBindingInvalid means local binding configuration cannot be decoded.
	PublicationBindingInvalid PublicationValidation = "publication_binding_invalid"
	// PublicationNotionOpenTargetRequired means no initial open status is configured.
	PublicationNotionOpenTargetRequired PublicationValidation = "publication_notion_open_target_required"
	// PublicationTwentyRequiredField means task creation needs an unsupported field.
	PublicationTwentyRequiredField PublicationValidation = "publication_twenty_required_field"
	// PublicationGitHubContentInvalid means content violates GitHub create limits.
	PublicationGitHubContentInvalid PublicationValidation = "publication_github_content_invalid"
	// PublicationContentInvalid means content requires valid UTF-8, no NULs, and bounded title and body.
	PublicationContentInvalid PublicationValidation = "publication_content_invalid"
	// PublicationLinearContentInvalid means Linear content requires a bounded description.
	PublicationLinearContentInvalid PublicationValidation = "publication_linear_content_invalid"
	// PublicationPlaneContentInvalid means Plane content requires bounded title and escaped HTML.
	PublicationPlaneContentInvalid PublicationValidation = "publication_plane_content_invalid"
	// PublicationNotionContentInvalid means Notion content requires bounded title and serialized payload.
	PublicationNotionContentInvalid PublicationValidation = "publication_notion_content_invalid"
	// PublicationDestinationUnavailable means the destination is unavailable or mismatched.
	PublicationDestinationUnavailable PublicationValidation = "publication_destination_unavailable"
	// PublicationWritePermissionRequired means the destination explicitly lacks write permission.
	PublicationWritePermissionRequired PublicationValidation = "publication_write_permission_required"
	// PublicationOpenTargetRequired means the destination has no valid initial open status.
	PublicationOpenTargetRequired PublicationValidation = "publication_open_target_required"
	// PublicationSchemaInvalid means the live destination schema no longer supports the binding.
	PublicationSchemaInvalid PublicationValidation = "publication_schema_invalid"
	// PublicationOpenIssueRequired means creation requires an open native issue.
	PublicationOpenIssueRequired PublicationValidation = "publication_open_issue_required"
)

type publicationValidationError struct{ reason PublicationValidation }

// NewPublicationValidationError accepts only catalog identifiers, never provider
// diagnostics, field names, credentials, or issue content.
func NewPublicationValidationError(reason PublicationValidation) error {
	return &publicationValidationError{reason: reason}
}

func (e *publicationValidationError) Error() string {
	message, _, ok := publicationValidationDetails(string(e.reason))
	if !ok {
		return "publication validation failed"
	}
	return message
}

func publicationValidationDetails(code string) (message, hint string, ok bool) {
	switch PublicationValidation(code) {
	case PublicationBindingInvalid:
		return "The publication binding configuration is invalid.", "Check the daemon provider origin and credential account, then run sync enable with valid source and status settings and publish again.", true
	case PublicationNotionOpenTargetRequired:
		return "Notion publication requires an open status target.", "Re-enable Notion sync with workflow groups and --todo-group; use --open-status to select the initial status when needed.", true
	case PublicationTwentyRequiredField:
		return "Twenty requires an additional task field before publication.", "Make the additional required task field optional or give it a default in Twenty, then publish again.", true
	case PublicationGitHubContentInvalid:
		return "GitHub publication content is invalid or exceeds its limits.", "Use valid UTF-8, a nonempty title of at most 256 characters, and a body of at most 65536 characters, then publish again.", true
	case PublicationContentInvalid:
		return "Publication content is invalid or exceeds its limits.", "Use valid UTF-8 without NUL characters, a nonempty title, and a title and body of at most 1 MiB each, then publish again.", true
	case PublicationLinearContentInvalid:
		return "Linear publication content is invalid or exceeds its limits.", "Use valid UTF-8 without NUL characters, a nonempty title, and a description of at most 1 MiB, then publish again.", true
	case PublicationPlaneContentInvalid:
		return "Plane publication content is invalid or exceeds its limits.", "Use valid UTF-8 without NUL characters, a nonempty title of at most 255 characters, and a body whose escaped HTML fits 1 MiB, then publish again.", true
	case PublicationNotionContentInvalid:
		return "Notion publication content is invalid or exceeds its limits.", "Use valid UTF-8, a nonempty title of at most 200000 characters, and shorten the title or body until the serialized request fits 500 KiB, then publish again.", true
	case PublicationDestinationUnavailable:
		return "The publication destination is unavailable or does not match the binding.", "Restore an active destination with creation enabled, or re-enable sync against the intended source, then publish again.", true
	case PublicationWritePermissionRequired:
		return "The publication destination requires write permission.", "Grant the daemon credential create/write access to the selected project, then publish again.", true
	case PublicationOpenTargetRequired:
		return "The publication destination has no valid open status target.", "Add an open status to the destination workflow or update the binding's open status selection, then publish again.", true
	case PublicationSchemaInvalid:
		return "The publication destination schema does not support the configured fields or statuses.", "Restore the destination's required fields and statuses or re-enable sync with current field and status mappings, then publish again.", true
	case PublicationOpenIssueRequired:
		return "Publication requires an open native issue.", "Reopen the native issue before publishing it.", true

	default:
		return "", "", false
	}
}

// ValidationDetails returns only allowlisted local validation text.
func (e *PublishError) ValidationDetails() (message, hint string, ok bool) {
	return publicationValidationDetails(e.Code)
}

func publicationValidationCode(err error, fallback string) string {
	if validation, ok := errors.AsType[*publicationValidationError](err); ok {
		code := string(validation.reason)
		if _, _, known := publicationValidationDetails(code); known {
			return code
		}
	}
	return fallback
}
