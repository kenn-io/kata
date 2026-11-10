package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/metadata"
)

// validateScheduleOutput rejects unreadable rules before a tool returns a summary.
func validateScheduleOutput[In, Out any](handler sdkmcp.ToolHandlerFor[In, Out]) sdkmcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest, input In) (*sdkmcp.CallToolResult, Out, error) {
		result, output, err := handler(ctx, request, input)
		if err == nil {
			err = scheduleOutputError(reflect.ValueOf(output))
		}
		return result, output, err
	}
}

func scheduleOutputError(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	if value.Type() == reflect.TypeFor[IssueSummary]() && value.CanInterface() {
		summary := value.Interface().(IssueSummary)
		for _, field := range []string{"scheduled_on", "deadline_on"} {
			raw, err := json.Marshal(summary.metadata[field])
			if err == nil {
				_, err = metadata.ScheduleValueText(raw)
			}
			if errors.Is(err, metadata.ErrUnsupportedScheduleTZData) {
				return err
			}
		}
		return nil
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			return scheduleOutputError(value.Elem())
		}
	case reflect.Struct:
		for _, field := range value.Fields() {
			if field.CanInterface() {
				if err := scheduleOutputError(field); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			if err := scheduleOutputError(value.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}
