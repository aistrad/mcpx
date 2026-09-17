package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"mcpx/internal/mcpresult"
)

func schemaErrorField(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	if strings.HasPrefix(message, "missing required field ") {
		value := strings.TrimPrefix(message, "missing required field ")
		return strings.Trim(value, " '\"")
	}
	if index := strings.Index(message, " unknown field "); index >= 0 {
		prefix := strings.TrimPrefix(message[:index], "arguments.")
		field := strings.Trim(strings.TrimPrefix(message[index:], " unknown field "), " '\"")
		if prefix != "" && field != "" {
			return prefix + "." + field
		}
		return field
	}
	field := message
	if index := strings.IndexAny(field, " '\""); index >= 0 {
		field = field[:index]
	}
	field = strings.TrimPrefix(field, "arguments.")
	if field == message || field == "" {
		return ""
	}
	return field
}

// validateRegisteredToolArguments applies the same published input schema to
// direct handler calls and to MCP transport calls. The official MCP SDK
// validates transport payloads, but direct/operation paths must not silently
// accept fields that the published contract rejects.
func (r *Runtime) validateRegisteredToolArguments(name string, arguments map[string]any) error {
	return r.validateRegisteredToolArgumentsContext(context.Background(), name, arguments)
}

// validateRegisteredToolArgumentsContext applies the published contract while
// retaining the small set of fields that Runtime injects when it resumes an
// operation child. Those fields are transport metadata, not client-visible
// business arguments; direct calls remain strict through the wrapper above.
func (r *Runtime) validateRegisteredToolArgumentsContext(ctx context.Context, name string, arguments map[string]any) error {
	if r == nil {
		return nil
	}
	r.toolIndexMu.RLock()
	tool, ok := r.toolIndex[name]
	r.toolIndexMu.RUnlock()
	if !ok {
		// Tests and internal callers may instrument an isolated handler without
		// registering it in the public catalog.
		return nil
	}
	raw := mcpresult.ToolSchemaJSON(tool)
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return fmt.Errorf("invalid published schema for tool %q", name)
	}
	if err := validatePublishedShape(arguments, schema, "arguments", isOperationChild(ctx)); err != nil {
		return err
	}
	if err := validateOperationSchemaValue(arguments, schema, "arguments"); err != nil {
		// Branch-specific required fields and semantic constraints are owned by
		// the handler so it can return a precise recovery action. The transport
		// schema still catches unknown fields and malformed nested values here.
		if strings.Contains(err.Error(), "does not match any supported schema branch") || strings.HasPrefix(err.Error(), "missing required field") || strings.Contains(err.Error(), "has an unsupported value") {
			return nil
		}
		return err
	}
	return nil
}

// validatePublishedShape checks the part of JSON Schema that must be enforced
// before a handler runs even when the schema contains oneOf branches. The
// existing operation validator intentionally lets handlers report branch
// specific required/enum errors; this pass still rejects unknown fields in the
// union of all applicable branches.
func validatePublishedShape(value any, schema map[string]any, path string, allowOperationConfirmation bool) error {
	if branches, ok := schema["oneOf"].([]any); ok && len(branches) > 0 {
		if object, ok := value.(map[string]any); ok {
			for _, raw := range branches {
				branch, ok := raw.(map[string]any)
				if !ok || !publishedBranchMatches(object, branch) {
					continue
				}
				properties, _ := branch["properties"].(map[string]any)
				additional, explicit := branch["additionalProperties"].(bool)
				return validateObjectShape(value, propertyMaps(properties), explicit && !additional, path, allowOperationConfirmation)
			}
		}
		union := map[string]map[string]any{}
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if properties, ok := branch["properties"].(map[string]any); ok {
				for key, rawProperty := range properties {
					if property, ok := rawProperty.(map[string]any); ok {
						union[key] = property
					}
				}
			}
		}
		return validateObjectShape(value, union, true, path, allowOperationConfirmation)
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties != nil {
		additional, explicit := schema["additionalProperties"].(bool)
		if err := validateObjectShape(value, propertyMaps(properties), explicit && !additional, path, allowOperationConfirmation); err != nil {
			return err
		}
	}
	return validatePublishedType(value, schema, path)
}

// publishedBranchMatches selects a discriminated oneOf branch when the
// request already contains its action or operation target. Without a selected
// branch, the union check below still catches fields that no branch publishes;
// the handler remains responsible for missing required fields and bad action
// values so it can return its normal recovery response.
func publishedBranchMatches(object map[string]any, branch map[string]any) bool {
	// Leave mutually-exclusive operation target forms to parseOperationTargets
	// so callers receive its precise semantic error instead of a branch-shape
	// error that hides the actual correction.
	if _, hasID := object["operation_id"]; hasID {
		if _, hasIDs := object["operation_ids"]; hasIDs {
			return false
		}
	}
	properties, _ := branch["properties"].(map[string]any)
	if action, ok := object["action"]; ok {
		if actionSchema, ok := properties["action"].(map[string]any); ok {
			if constant, exists := actionSchema["const"]; exists {
				return fmt.Sprint(constant) == fmt.Sprint(action)
			}
		}
	}
	required, _ := branch["required"].([]any)
	for _, raw := range required {
		key, _ := raw.(string)
		if key == "operation_ids" {
			_, present := object[key]
			return present
		}
		if key == "operation_id" {
			_, present := object[key]
			return present
		}
	}
	return false
}

// validatePublishedType enforces the structural part of the published
// schema. Branch-specific required/enum/const decisions remain with the
// handler, but malformed primitive, array, and object values must be rejected
// before a direct or resumed call can have side effects.
func validatePublishedType(value any, schema map[string]any, path string) error {
	schemaType, _ := schema["type"].(string)
	if schemaType == "" {
		if _, hasProperties := schema["properties"]; hasProperties {
			schemaType = "object"
		}
	}
	switch schemaType {
	case "object":
		if !isObjectValue(value) {
			return fmt.Errorf("%s must be an object", path)
		}
	case "array":
		if !isArrayValue(value) {
			return fmt.Errorf("%s must be an array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "number":
		if !isNumberValue(value) {
			return fmt.Errorf("%s must be a number", path)
		}
	case "integer":
		if !isIntegerValue(value) {
			return fmt.Errorf("%s must be an integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	}
	return nil
}

func isObjectValue(value any) bool {
	if value == nil {
		return false
	}
	_, ok := value.(map[string]any)
	return ok
}

func isArrayValue(value any) bool {
	if value == nil {
		return false
	}
	kind := reflect.TypeOf(value).Kind()
	return kind == reflect.Array || kind == reflect.Slice
}

func isNumberValue(value any) bool {
	if value == nil {
		return false
	}
	switch reflect.TypeOf(value).Kind() {
	case reflect.Float32, reflect.Float64, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

func isIntegerValue(value any) bool {
	if value == nil {
		return false
	}
	typ := reflect.TypeOf(value)
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Float32, reflect.Float64:
		floatValue := reflect.ValueOf(value).Float()
		return floatValue == floatValue && floatValue == float64(int64(floatValue))
	default:
		return false
	}
}

func propertyMaps(properties map[string]any) map[string]map[string]any {
	result := make(map[string]map[string]any, len(properties))
	for key, raw := range properties {
		if property, ok := raw.(map[string]any); ok {
			result[key] = property
		}
	}
	return result
}

func validateObjectShape(value any, properties map[string]map[string]any, rejectUnknown bool, path string, allowOperationConfirmation bool) error {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, item := range object {
		schema, known := properties[key]
		if !known {
			if isOperationInjectedField(key, allowOperationConfirmation) {
				continue
			}
			if rejectUnknown {
				return fmt.Errorf("%s unknown field %q", path, key)
			}
			continue
		}
		// Runtime-injected operation metadata is legal only on the top-level
		// tool arguments. Nested business objects remain fully strict.
		if err := validatePublishedShape(item, schema, path+"."+key, false); err != nil {
			return err
		}
		if items, ok := schema["items"].(map[string]any); ok {
			value := reflect.ValueOf(item)
			if value.IsValid() && (value.Kind() == reflect.Array || value.Kind() == reflect.Slice) {
				for index := 0; index < value.Len(); index++ {
					if err := validatePublishedShape(value.Index(index).Interface(), items, fmt.Sprintf("%s.%s[%d]", path, key, index), false); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
