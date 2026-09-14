package tool

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// validateArguments enforces the same JSON Schema contract that is advertised
// to the model. Validation happens before permission matching and approval, so
// malformed input can neither influence a rule decision nor reach a tool.
//
// Tool.Parameters is the schema's properties map rather than a complete root
// schema. The root is intentionally closed: unknown top-level arguments are
// rejected as likely model/tool-version mistakes. Nested objects follow JSON
// Schema's normal default (additional properties allowed) unless they declare
// additionalProperties=false.
func validateArguments(t Tool, args map[string]any) error {
	props := t.Parameters()
	if props == nil {
		props = map[string]any{}
	}
	required := []string{}
	if rp, ok := t.(RequiredParams); ok {
		required = rp.Required()
	}
	root := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	return validateSchemaValue("arguments", args, root)
}

func validateSchemaValue(path string, value any, schema map[string]any) error {
	if enum, ok := schema["enum"].([]any); ok && !enumContains(enum, value) {
		return fmt.Errorf("%s must be one of %s", path, renderEnum(enum))
	}
	if enum, ok := schema["enum"].([]string); ok {
		values := make([]any, len(enum))
		for i := range enum {
			values[i] = enum[i]
		}
		if !enumContains(values, value) {
			return fmt.Errorf("%s must be one of %s", path, renderEnum(values))
		}
	}
	if want, ok := schema["const"]; ok && !reflect.DeepEqual(want, value) {
		return fmt.Errorf("%s must equal %v", path, want)
	}
	if branches, ok := schemaList(schema["oneOf"]); ok {
		return validateSchemaBranches(path, value, branches, true)
	}
	if branches, ok := schemaList(schema["anyOf"]); ok {
		return validateSchemaBranches(path, value, branches, false)
	}

	types := schemaTypes(schema["type"])
	if len(types) > 0 && !matchesAnyType(value, types) {
		return fmt.Errorf("%s must be %s, got %s", path, strings.Join(types, " or "), jsonType(value))
	}

	switch v := value.(type) {
	case map[string]any:
		return validateObject(path, v, schema)
	case []any:
		if n, ok := numberKeyword(schema, "minItems"); ok && float64(len(v)) < n {
			return fmt.Errorf("%s must contain at least %d items", path, int(n))
		}
		if n, ok := numberKeyword(schema, "maxItems"); ok && float64(len(v)) > n {
			return fmt.Errorf("%s must contain at most %d items", path, int(n))
		}
		if itemSchema, ok := asSchema(schema["items"]); ok {
			for i, item := range v {
				if err := validateSchemaValue(fmt.Sprintf("%s[%d]", path, i), item, itemSchema); err != nil {
					return err
				}
			}
		}
	case string:
		runes := len([]rune(v))
		if n, ok := numberKeyword(schema, "minLength"); ok && float64(runes) < n {
			return fmt.Errorf("%s is shorter than minLength %d", path, int(n))
		}
		if n, ok := numberKeyword(schema, "maxLength"); ok && float64(runes) > n {
			return fmt.Errorf("%s is longer than maxLength %d", path, int(n))
		}
		if pattern, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("%s has invalid schema pattern: %w", path, err)
			}
			if !re.MatchString(v) {
				return fmt.Errorf("%s does not match pattern %q", path, pattern)
			}
		}
	case float64:
		if n, ok := numberKeyword(schema, "minimum"); ok && v < n {
			return fmt.Errorf("%s must be >= %v", path, n)
		}
		if n, ok := numberKeyword(schema, "maximum"); ok && v > n {
			return fmt.Errorf("%s must be <= %v", path, n)
		}
	}
	return nil
}

func validateObject(path string, value map[string]any, schema map[string]any) error {
	required := stringList(schema["required"])
	for _, name := range required {
		if v, ok := value[name]; !ok || v == nil {
			return fmt.Errorf("%s.%s is required", path, name)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(value))
	for name := range value {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		prop, declared := props[name]
		if !declared {
			if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
				return fmt.Errorf("%s.%s is not an allowed argument", path, name)
			}
			continue
		}
		propSchema, ok := asSchema(prop)
		if !ok {
			continue
		}
		if err := validateSchemaValue(path+"."+name, value[name], propSchema); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaBranches(path string, value any, branches []map[string]any, exactlyOne bool) error {
	matches := 0
	for _, branch := range branches {
		if validateSchemaValue(path, value, branch) == nil {
			matches++
		}
	}
	if matches == 0 || (exactlyOne && matches != 1) {
		kind := "anyOf"
		if exactlyOne {
			kind = "oneOf"
		}
		return fmt.Errorf("%s does not satisfy schema %s", path, kind)
	}
	return nil
}

func schemaTypes(raw any) []string {
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}

func matchesAnyType(value any, types []string) bool {
	for _, typ := range types {
		switch typ {
		case "null":
			if value == nil {
				return true
			}
		case "object":
			_, ok := value.(map[string]any)
			if ok {
				return true
			}
		case "array":
			_, ok := value.([]any)
			if ok {
				return true
			}
		case "string":
			_, ok := value.(string)
			if ok {
				return true
			}
		case "number":
			_, ok := value.(float64)
			if ok {
				return true
			}
		case "integer":
			v, ok := value.(float64)
			if ok && !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v {
				return true
			}
		case "boolean":
			_, ok := value.(bool)
			if ok {
				return true
			}
		}
	}
	return false
}

func jsonType(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.Trunc(v) == v {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func asSchema(raw any) (map[string]any, bool) {
	if m, ok := raw.(map[string]any); ok {
		return m, true
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return m, true
}

func schemaList(raw any) ([]map[string]any, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := asSchema(item)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, len(out) > 0
}

func stringList(raw any) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func numberKeyword(schema map[string]any, name string) (float64, bool) {
	switch n := schema[name].(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func enumContains(enum []any, value any) bool {
	for _, item := range enum {
		if reflect.DeepEqual(item, value) {
			return true
		}
	}
	return false
}

func renderEnum(enum []any) string {
	b, err := json.Marshal(enum)
	if err != nil {
		return fmt.Sprint(enum)
	}
	return string(b)
}
