package daemon

import (
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

func newAPISchemaRegistry() huma.Registry {
	return &apiSchemaRegistry{
		Registry: huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer),
	}
}

type apiSchemaRegistry struct {
	huma.Registry
}

func (r *apiSchemaRegistry) MarshalJSON() ([]byte, error) {
	return jsonv1.Marshal(r.Registry)
}

func (r *apiSchemaRegistry) MarshalYAML() (any, error) {
	return r.Map(), nil
}

func (r *apiSchemaRegistry) Schema(t reflect.Type, allowRef bool, hint string) *huma.Schema {
	schema := r.Registry.Schema(t, allowRef, hint)
	for _, registered := range r.Map() {
		makeArraysNonNullable(registered, map[*huma.Schema]bool{})
	}
	makeArraysNonNullable(schema, map[*huma.Schema]bool{})
	return schema
}

func makeArraysNonNullable(schema *huma.Schema, seen map[*huma.Schema]bool) {
	if schema == nil || seen[schema] {
		return
	}
	seen[schema] = true
	if schema.Type == huma.TypeArray {
		schema.Nullable = false
	}
	makeArraysNonNullable(schema.Items, seen)
	makeArraysNonNullable(schema.Not, seen)
	for _, property := range schema.Properties {
		makeArraysNonNullable(property, seen)
	}
	for _, alternative := range schema.OneOf {
		makeArraysNonNullable(alternative, seen)
	}
	for _, alternative := range schema.AnyOf {
		makeArraysNonNullable(alternative, seen)
	}
	for _, alternative := range schema.AllOf {
		makeArraysNonNullable(alternative, seen)
	}
	if additional, ok := schema.AdditionalProperties.(*huma.Schema); ok {
		makeArraysNonNullable(additional, seen)
	}
}

func unmarshalAPIJSON(data []byte, value any) error {
	if err := jsonv2.Unmarshal(data, value); err != nil {
		return err
	}

	target := reflect.TypeOf(value)
	if target == nil {
		return nil
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() == reflect.Interface {
		return nil
	}

	var decoded any
	if err := jsonv2.Unmarshal(data, &decoded); err != nil {
		return err
	}
	return rejectNullArrays(decoded, target, "body")
}

func rejectNullArrays(value any, target reflect.Type, path string) error {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}

	if target.Kind() == reflect.Slice && target.Elem().Kind() != reflect.Uint8 {
		if value == nil {
			return fmt.Errorf("%s: null is not allowed for an array", path)
		}
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for i, item := range items {
			if err := rejectNullArrays(item, target.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}

	if target.Kind() == reflect.Array {
		if value == nil {
			return fmt.Errorf("%s: null is not allowed for an array", path)
		}
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for i, item := range items {
			if err := rejectNullArrays(item, target.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}

	if target.Kind() != reflect.Struct || value == nil {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for name, member := range object {
		fieldType, ok := jsonFieldType(target, name)
		if !ok {
			if allowsAdditionalJSONProperties(target) {
				continue
			}
			return fmt.Errorf("%s.%s: unknown object member", path, name)
		}
		if err := rejectNullArrays(member, fieldType, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func allowsAdditionalJSONProperties(target reflect.Type) bool {
	for field := range target.Fields() {
		if field.Tag.Get("additionalProperties") == "true" {
			return true
		}
	}
	return false
}

func jsonFieldType(target reflect.Type, name string) (reflect.Type, bool) {
	for field := range target.Fields() {
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		parts := strings.Split(tag, ",")
		if parts[0] == "-" {
			continue
		}
		inline := field.Anonymous && parts[0] == ""
		for _, option := range parts[1:] {
			inline = inline || option == "inline"
		}
		if inline {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				if fieldType, ok := jsonFieldType(embedded, name); ok {
					return fieldType, true
				}
			}
			continue
		}
		fieldName := parts[0]
		if fieldName == "" {
			fieldName = field.Name
		}
		if fieldName == name {
			return field.Type, true
		}
	}
	return nil, false
}
