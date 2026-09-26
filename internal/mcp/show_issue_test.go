package mcpserver

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// TestIssueFromShowCopiesEveryIssueField fills every field of the show-issue
// record with a non-zero value and checks each generated.Issue field arrives
// intact, so a field added to the API later cannot be silently dropped.
func TestIssueFromShowCopiesEveryIssueField(t *testing.T) {
	var shown generated.ShowIssueOut
	fillNonZero(t, reflect.ValueOf(&shown).Elem())

	issue := issueFromShow(shown)

	issueValue := reflect.ValueOf(issue)
	shownValue := reflect.ValueOf(shown)
	for i := range issueValue.NumField() {
		name := issueValue.Type().Field(i).Name
		source := shownValue.FieldByName(name)
		require.Truef(t, source.IsValid(), "ShowIssueOut lacks Issue field %s", name)
		require.Equalf(t, source.Interface(), issueValue.Field(i).Interface(), "field %s", name)
	}
}

func fillNonZero(t *testing.T, value reflect.Value) {
	t.Helper()
	for i := range value.NumField() {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString("value-" + value.Type().Field(i).Name)
		case reflect.Int64:
			field.SetInt(int64(i + 1))
		case reflect.Map:
			field.Set(reflect.ValueOf(map[string]any{"key": "value"}))
		case reflect.Slice:
			field.Set(reflect.ValueOf([]string{"label"}))
		case reflect.Pointer:
			elem := reflect.New(field.Type().Elem())
			switch elem.Elem().Kind() {
			case reflect.String:
				elem.Elem().SetString("pointer-" + value.Type().Field(i).Name)
			case reflect.Int64:
				elem.Elem().SetInt(int64(100 + i))
			case reflect.Struct:
				elem.Elem().Set(reflect.ValueOf(time.Unix(int64(1000+i), 0).UTC()))
			default:
				t.Fatalf("unsupported pointer field %s", value.Type().Field(i).Name)
			}
			field.Set(elem)
		case reflect.Struct:
			field.Set(reflect.ValueOf(time.Unix(int64(2000+i), 0).UTC()))
		default:
			t.Fatalf("unsupported field %s of kind %s", value.Type().Field(i).Name, field.Kind())
		}
	}
}
