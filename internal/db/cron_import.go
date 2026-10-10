package db

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

func cronUID(value string) bool { return uid.Valid(value) && value == strings.ToUpper(value) }

// ValidateCronRecord checks one backup row before either backend restores it.
// Definition documents were decoded strictly with the record; today's rules are
// not re-applied, so a backup taken under older rules still restores.
func ValidateCronRecord(record ImportRecord) error {
	switch value := record.(type) {
	case *CronJobExport:
		if value == nil {
			return fmt.Errorf("nil cron job")
		}
		return validateCronDefinition(value.CronDefinition)
	case *CronWorkflowExport:
		if value == nil {
			return fmt.Errorf("nil cron workflow")
		}
		return validateCronDefinition(value.CronDefinition)
	case *CronRunExport:
		if value == nil {
			return fmt.Errorf("nil cron run")
		}
		if value.ID <= 0 || value.Revision < 1 || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
			return fmt.Errorf("invalid cron observation history")
		}
		return validateCronRun(CronRun(*value))
	default:
		return fmt.Errorf("unknown cron record %T", record)
	}
}
func validateCronDefinition(value CronDefinition) error {
	if value.ID <= 0 || value.ProjectID <= 0 || !cronUID(value.UID) || !cronUID(value.DefinitionEventUID) || value.Revision < 1 || strings.TrimSpace(value.Name) == "" || len(value.Name) > 256 || strings.TrimSpace(value.Author) == "" || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid cron definition identity")
	}
	return value.DefinitionHLC.Validate()
}
