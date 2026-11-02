package config

import (
	"encoding/json"
	"reflect"
	"strings"
)

var durationType = reflect.TypeFor[Duration]()

// ServerSchema returns a JSON Schema for server.toml, made from the Go types.
// Editors use it through the "#:schema" line.
func ServerSchema() ([]byte, error) {
	s := schemaFor(reflect.TypeFor[Server]())
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["title"] = "Hostel server configuration (server.toml)"
	return json.MarshalIndent(s, "", "  ")
}

func schemaFor(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == durationType {
		return map[string]any{"type": "string", "pattern": `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Map:
		if t.Elem().Kind() == reflect.Interface {
			return map[string]any{"type": "object"}
		}
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if name == "" || name == "-" || f.Tag.Get("schema") == "-" {
				continue
			}
			props[name] = schemaFor(f.Type)
		}
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	}
	return map[string]any{}
}
